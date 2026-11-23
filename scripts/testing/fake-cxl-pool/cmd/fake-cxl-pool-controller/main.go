// Copyright The NRI Plugins Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// fake-cxl-pool-controller is the cluster side of the cxl-pool.nri.io DRA
// driver for a fake-cxl-pool server: it publishes the pool devices as a
// ResourceSlice for the nodes the server reaches, attaches a device to the VM of the node that the
// scheduler picked, writes the binding condition into the claim status,
// and detaches devices of deallocated claims. See README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"

	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/client"
	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/controller"
)

func main() {
	os.Exit(run())
}

// run runs the controller until a signal and returns the exit code.
func run() int {
	var cfg controller.Config
	server := flag.String("server", client.DefaultURL(), "fake-cxl-pool server URL (env FAKE_CXL_POOL_SERVER)")
	kubeconfig := flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "kubeconfig file (env KUBECONFIG; \"\": in-cluster config)")
	flag.StringVar(&cfg.DRAPoolName, "dra-pool-name", "", "DRA pool name of the published devices (default: the name of the server)")
	flag.DurationVar(&cfg.SyncInterval, "sync-interval", controller.DefaultSyncInterval, "interval of full reconciles")
	flag.DurationVar(&cfg.AttachTimeout, "attach-timeout", controller.DefaultAttachTimeout, "attach request timeout")
	flag.DurationVar(&cfg.DetachTimeout, "detach-timeout", controller.DefaultDetachTimeout, "detach request timeout")
	flag.StringVar(&cfg.OwnerPrefix, "owner-prefix", controller.DefaultOwnerPrefix, "prefix of the attachment owners of this controller")
	flag.StringVar(&cfg.ControllerKey, "controller-key", controller.DefaultControllerKey, "key of the controller's settings in pool.opaque of MemoryConfig entries")
	flag.BoolVar(&cfg.Verbose, "v", false, "log reconcile details")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 0 {
		flag.Usage()
		return 2
	}
	logger := log.New(os.Stderr, "fake-cxl-pool-controller: ", log.LstdFlags|log.Lmicroseconds)
	cfg.Logger = logger

	var (
		rc  *rest.Config
		err error
	)
	if *kubeconfig == "" {
		rc, err = rest.InClusterConfig()
	} else {
		rc, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	}
	if err != nil {
		logger.Printf("kubernetes client config: %v", err)
		return 1
	}
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		logger.Printf("kubernetes client: %v", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	broadcaster := record.NewBroadcaster(record.WithContext(ctx))
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kube.CoreV1().Events("")})
	defer broadcaster.Shutdown()
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controller.Component})

	c := controller.New(cfg, kube, client.New(*server), recorder)
	if err := c.Run(ctx); err != nil {
		logger.Printf("%v", err)
		return 1
	}
	logger.Printf("stopped")
	return 0
}
