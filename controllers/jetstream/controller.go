// Copyright 2020-2022 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package jetstream

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nats-io/jsm.go"
	jsmapi "github.com/nats-io/jsm.go/api"
	"github.com/nats-io/nats.go"

	apis "github.com/nats-io/nack/pkg/jetstream/apis/jetstream/v1beta2"
	clientset "github.com/nats-io/nack/pkg/jetstream/generated/clientset/versioned"
	scheme "github.com/nats-io/nack/pkg/jetstream/generated/clientset/versioned/scheme"
	typed "github.com/nats-io/nack/pkg/jetstream/generated/clientset/versioned/typed/jetstream/v1beta2"
	informers "github.com/nats-io/nack/pkg/jetstream/generated/informers/externalversions"
	listers "github.com/nats-io/nack/pkg/jetstream/generated/listers/jetstream/v1beta2"

	k8sapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8smeta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	k8styped "k8s.io/client-go/kubernetes/typed/core/v1"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	klog "k8s.io/klog/v2"
)

const (
	// maxQueueRetries is the max times an item will be retried. An item will
	// be pulled maxQueueRetries+1 times from the queue. On pull number
	// maxQueueRetries+1, if it fails again, it won't be retried.
	maxQueueRetries = 10

	// readyCondType is the Ready condition type.
	readyCondType = "Ready"
)

type Options struct {
	Ctx context.Context

	KubeIface      kubernetes.Interface
	JetstreamIface clientset.Interface

	NATSClientName  string
	NATSCredentials string
	NATSNKey        string
	NATSServerURL   string

	NATSCA          string
	NATSCertificate string
	NATSKey         string

	Namespace     string
	CRDConnect    bool
	CleanupPeriod time.Duration

	Recorder record.EventRecorder
}

type Controller struct {
	ctx  context.Context
	opts Options
	nc   *nats.Conn
	jm   *jsm.Manager

	ki              k8styped.CoreV1Interface
	ji              typed.JetstreamV1beta2Interface
	informerFactory informers.SharedInformerFactory
	rec             record.EventRecorder

	strLister listers.StreamLister
	strSynced cache.InformerSynced
	strQueue  workqueue.RateLimitingInterface

	cnsLister listers.ConsumerLister
	cnsSynced cache.InformerSynced
	cnsQueue  workqueue.RateLimitingInterface

	accLister listers.AccountLister

	// cacheDir is where the downloaded TLS certs from the server
	// will be stored temporarily.
	cacheDir string
}

func NewController(opt Options) *Controller {
	resyncPeriod := 30 * time.Second
	informerFactory := informers.NewSharedInformerFactoryWithOptions(opt.JetstreamIface, resyncPeriod, informers.WithNamespace(opt.Namespace))

	streamInformer := informerFactory.Jetstream().V1beta2().Streams()
	consumerInformer := informerFactory.Jetstream().V1beta2().Consumers()
	accountInformer := informerFactory.Jetstream().V1beta2().Accounts()

	if opt.Recorder == nil {
		utilruntime.Must(scheme.AddToScheme(k8sscheme.Scheme))
		eventBroadcaster := record.NewBroadcaster()
		eventBroadcaster.StartLogging(klog.Infof)
		eventBroadcaster.StartRecordingToSink(&k8styped.EventSinkImpl{
			Interface: opt.KubeIface.CoreV1().Events(""),
		})

		opt.Recorder = eventBroadcaster.NewRecorder(k8sscheme.Scheme, k8sapi.EventSource{
			Component: "jetstream-controller",
		})
	}

	if opt.NATSClientName == "" {
		opt.NATSClientName = "jetstream-controller"
	}

	ji := opt.JetstreamIface.JetstreamV1beta2()
	streamQueue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Streams")
	consumerQueue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Consumers")

	streamInformer.Informer().AddEventHandler(eventHandlers(
		opt.Ctx,
		streamQueue,
	))

	consumerInformer.Informer().AddEventHandler(eventHandlers(
		opt.Ctx,
		consumerQueue,
	))

	cacheDir, err := os.MkdirTemp(".", "nack")
	if err != nil {
		panic(err)
	}

	return &Controller{
		ctx:  opt.Ctx,
		opts: opt,

		ki:              opt.KubeIface.CoreV1(),
		ji:              ji,
		informerFactory: informerFactory,
		rec:             opt.Recorder,

		strLister: streamInformer.Lister(),
		strSynced: streamInformer.Informer().HasSynced,
		strQueue:  streamQueue,

		cnsLister: consumerInformer.Lister(),
		cnsSynced: consumerInformer.Informer().HasSynced,
		cnsQueue:  consumerQueue,

		accLister: accountInformer.Lister(),
		cacheDir:  cacheDir,
	}
}

func (c *Controller) Run() error {
	if !c.opts.CRDConnect {
		// Connect to NATS.
		opts := make([]nats.Option, 0)

		opts = append(opts, nats.Name(c.opts.NATSClientName))

		// Use JWT/NKEYS based credentials if present.
		if c.opts.NATSCredentials != "" {
			opts = append(opts, nats.UserCredentials(c.opts.NATSCredentials))
		} else if c.opts.NATSNKey != "" {
			opt, err := nats.NkeyOptionFromSeed(c.opts.NATSNKey)
			if err != nil {
				return nil
			}
			opts = append(opts, opt)
		}

		if c.opts.NATSCertificate != "" && c.opts.NATSKey != "" {
			opts = append(opts, nats.ClientCert(c.opts.NATSCertificate, c.opts.NATSKey))
		}

		if c.opts.NATSCA != "" {
			opts = append(opts, nats.RootCAs(c.opts.NATSCA))
		}

		// Always attempt to have a connection to NATS.
		opts = append(opts, nats.MaxReconnects(-1))

		nc, err := nats.Connect(c.opts.NATSServerURL, opts...)
		if err != nil {
			return fmt.Errorf("failed to connect to nats: %w", err)
		}
		c.nc = nc
		jm, err := jsm.New(c.nc)
		if err != nil {
			return err
		}
		c.jm = jm
	}

	defer utilruntime.HandleCrash()

	defer c.strQueue.ShutDown()
	defer c.cnsQueue.ShutDown()

	c.informerFactory.Start(c.ctx.Done())

	if !cache.WaitForCacheSync(c.ctx.Done(), c.strSynced) {
		return fmt.Errorf("failed to wait for stream cache sync")
	}
	if !cache.WaitForCacheSync(c.ctx.Done(), c.cnsSynced) {
		return fmt.Errorf("failed to wait for consumer cache sync")
	}

	go wait.Until(c.runStreamQueue, time.Second, c.ctx.Done())
	go wait.Until(c.runConsumerQueue, time.Second, c.ctx.Done())
	go c.cleanupStreams()
	go c.cleanupConsumers()

	<-c.ctx.Done()

	// Gracefully shutdown.
	return nil
}

func deletedStreams(prev, cur map[string]*apis.Stream) []*apis.Stream {
	var deleted []*apis.Stream
	for name, ps := range prev {
		if _, ok := cur[name]; !ok {
			deleted = append(deleted, ps)
		}
	}
	return deleted
}

func streamMap(ss []*apis.Stream) map[string]*apis.Stream {
	m := make(map[string]*apis.Stream)
	for _, s := range ss {
		m[fmt.Sprintf("%s/%s", s.Namespace, s.Name)] = s
	}
	return m
}

func (c *Controller) cleanupStreams() error {
	tick := time.NewTicker(c.opts.CleanupPeriod)
	defer tick.Stop()

	var prevStreams map[string]*apis.Stream
	for {
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-tick.C:
			streams, err := c.strLister.List(labels.Everything())
			if err != nil {
				klog.Infof("failed to list streams for cleanup: %s", err)
				continue
			}
			sm := streamMap(streams)

			for _, s := range deletedStreams(prevStreams, sm) {
				t := k8smeta.NewTime(time.Now())
				s.DeletionTimestamp = &t
				if err := c.processStreamObject(s, &realJsmClient{jm: c.jm}); err != nil {
					klog.Infof("failed to delete stream %s/%s: %s", s.Namespace, s.Name, err)
					continue
				}
				klog.Infof("deleted stream %s/%s", s.Namespace, s.Name)
			}
			prevStreams = sm
		}
	}
}

func deletedConsumers(prev, cur map[string]*apis.Consumer) []*apis.Consumer {
	var deleted []*apis.Consumer
	for name, ps := range prev {
		if _, ok := cur[name]; !ok {
			deleted = append(deleted, ps)
		}
	}
	return deleted
}

func consumerMap(cs []*apis.Consumer) map[string]*apis.Consumer {
	m := make(map[string]*apis.Consumer)
	for _, c := range cs {
		m[fmt.Sprintf("%s/%s", c.Namespace, c.Name)] = c
	}
	return m
}

func (c *Controller) cleanupConsumers() error {
	tick := time.NewTicker(c.opts.CleanupPeriod)
	defer tick.Stop()

	var prevConsumers map[string]*apis.Consumer
	for {
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-tick.C:
			consumers, err := c.cnsLister.List(labels.Everything())
			if err != nil {
				klog.Infof("failed to list consumers for cleanup: %s", err)
				continue
			}
			cm := consumerMap(consumers)

			for _, cns := range deletedConsumers(prevConsumers, cm) {
				t := k8smeta.NewTime(time.Now())
				cns.DeletionTimestamp = &t
				if err := c.processConsumerObject(cns, &realJsmClient{jm: c.jm}); err != nil {
					klog.Infof("failed to delete consumer %s/%s: %s", cns.Namespace, cns.Name, err)
					continue
				}
				klog.Infof("deleted consumer %s/%s", cns.Namespace, cns.Name)
			}
			prevConsumers = cm
		}
	}
}

func (c *Controller) normalEvent(o runtime.Object, reason, message string) {
	if c.rec != nil {
		c.rec.Event(o, k8sapi.EventTypeNormal, reason, message)
	}
}

func (c *Controller) warningEvent(o runtime.Object, reason, message string) {
	if c.rec != nil {
		c.rec.Event(o, k8sapi.EventTypeWarning, reason, message)
	}
}

func splitNamespaceName(item interface{}) (ns string, name string, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("failed to split namespace-name: %w", err)
		}
	}()

	key, ok := item.(string)
	if !ok {
		return "", "", fmt.Errorf("unexpected type: got=%T, want=%T", item, key)
	}

	ns, name, err = cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return "", "", err
	}

	return ns, name, nil
}

func getStorageType(s string) (jsmapi.StorageType, error) {
	switch s {
	case strings.ToLower(jsmapi.FileStorage.String()):
		return jsmapi.FileStorage, nil
	case strings.ToLower(jsmapi.MemoryStorage.String()):
		return jsmapi.MemoryStorage, nil
	default:
		return 0, fmt.Errorf("invalid jetstream storage option: %s", s)
	}
}

func enqueueWork(q workqueue.RateLimitingInterface, item interface{}) (err error) {
	key, err := cache.MetaNamespaceKeyFunc(item)
	if err != nil {
		return fmt.Errorf("failed to enqueue work: %w", err)
	}

	q.Add(key)
	return nil
}

type processorFunc func(ns, name string, c jsmClient) error

func processQueueNext(q workqueue.RateLimitingInterface, c jsmClient, process processorFunc) {
	item, shutdown := q.Get()
	if shutdown {
		return
	}
	defer q.Done(item)

	ns, name, err := splitNamespaceName(item)
	if err != nil {
		// Probably junk, clean it up.
		utilruntime.HandleError(err)
		q.Forget(item)
		return
	}

	err = process(ns, name, c)
	if err == nil {
		// Item processed successfully, don't requeue.
		q.Forget(item)
		return
	}

	utilruntime.HandleError(err)

	if q.NumRequeues(item) < maxQueueRetries {
		// Failed to process item, try again.
		q.AddRateLimited(item)
		return
	}

	// If we haven't been able to recover by this point, then just stop.
	// The user should have enough info in kubectl describe to debug.
	q.Forget(item)
}

func upsertCondition(cs []apis.Condition, next apis.Condition) []apis.Condition {
	for i := 0; i < len(cs); i++ {
		if cs[i].Type != next.Type {
			continue
		}

		cs[i] = next
		return cs
	}

	return append(cs, next)
}

func shouldEnqueue(prevObj, nextObj interface{}) bool {
	type crd interface {
		GetDeletionTimestamp() *k8smeta.Time
		GetSpec() interface{}
	}

	prev, ok := prevObj.(crd)
	if !ok {
		return false
	}

	next, ok := nextObj.(crd)
	if !ok {
		return false
	}

	markedDelete := next.GetDeletionTimestamp() != nil
	specChanged := !equality.Semantic.DeepEqual(prev.GetSpec(), next.GetSpec())

	return markedDelete || specChanged
}

func eventHandlers(ctx context.Context, q workqueue.RateLimitingInterface) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if err := enqueueWork(q, obj); err != nil {
				utilruntime.HandleError(err)
			}
		},
		UpdateFunc: func(prev, next interface{}) {
			if !shouldEnqueue(prev, next) {
				return
			}

			if err := enqueueWork(q, next); err != nil {
				utilruntime.HandleError(err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			if err := enqueueWork(q, obj); err != nil {
				utilruntime.HandleError(err)
			}
		},
	}
}
