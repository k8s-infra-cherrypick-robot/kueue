/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	kubeflow "github.com/kubeflow/mpi-operator/pkg/apis/kubeflow/v2beta1"
	zaplog "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	config "sigs.k8s.io/kueue/apis/config/v1beta1"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	"sigs.k8s.io/kueue/apis/kueue/webhooks"
	"sigs.k8s.io/kueue/pkg/cache"
	"sigs.k8s.io/kueue/pkg/constants"
	"sigs.k8s.io/kueue/pkg/controller/core"
	"sigs.k8s.io/kueue/pkg/controller/core/indexer"
	"sigs.k8s.io/kueue/pkg/controller/jobframework"
	"sigs.k8s.io/kueue/pkg/controller/jobs/job"
	"sigs.k8s.io/kueue/pkg/controller/jobs/mpijob"
	"sigs.k8s.io/kueue/pkg/controller/jobs/noop"
	"sigs.k8s.io/kueue/pkg/metrics"
	"sigs.k8s.io/kueue/pkg/queue"
	"sigs.k8s.io/kueue/pkg/scheduler"
	"sigs.k8s.io/kueue/pkg/util/cert"
	"sigs.k8s.io/kueue/pkg/util/useragent"
	"sigs.k8s.io/kueue/pkg/version"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(schedulingv1.AddToScheme(scheme))

	utilruntime.Must(kueue.AddToScheme(scheme))
	utilruntime.Must(config.AddToScheme(scheme))
	utilruntime.Must(kubeflow.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var configFile string
	flag.StringVar(&configFile, "config", "",
		"The controller will load its initial configuration from this file. "+
			"Omit this flag to use the default configuration values. ")

	opts := zap.Options{
		TimeEncoder: zapcore.RFC3339NanoTimeEncoder,
		ZapOpts:     []zaplog.Option{zaplog.AddCaller()},
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog.Info("Initializing", "gitVersion", version.GitVersion, "gitCommit", version.GitCommit)

	options, cfg := apply(configFile)

	metrics.Register()

	kubeConfig := ctrl.GetConfigOrDie()
	if kubeConfig.UserAgent == "" {
		kubeConfig.UserAgent = useragent.Default()
	}
	kubeConfig.QPS = *cfg.ClientConnection.QPS
	kubeConfig.Burst = int(*cfg.ClientConnection.Burst)
	setupLog.V(2).Info("K8S Client", "qps", kubeConfig.QPS, "burst", kubeConfig.Burst)
	mgr, err := ctrl.NewManager(kubeConfig, options)
	if err != nil {
		setupLog.Error(err, "Unable to start manager")
		os.Exit(1)
	}

	certsReady := make(chan struct{})

	if cfg.InternalCertManagement != nil && *cfg.InternalCertManagement.Enable {
		if err = cert.ManageCerts(mgr, cfg, certsReady); err != nil {
			setupLog.Error(err, "Unable to set up cert rotation")
			os.Exit(1)
		}
	} else {
		close(certsReady)
	}

	cCache := cache.New(mgr.GetClient(), cache.WithPodsReadyTracking(waitForPodsReady(&cfg)))
	queues := queue.NewManager(mgr.GetClient(), cCache)

	ctx := ctrl.SetupSignalHandler()
	setupIndexes(ctx, mgr, &cfg)

	setupProbeEndpoints(mgr)
	// Cert won't be ready until manager starts, so start a goroutine here which
	// will block until the cert is ready before setting up the controllers.
	// Controllers who register after manager starts will start directly.
	go setupControllers(mgr, cCache, queues, certsReady, &cfg)

	go func() {
		queues.CleanUpOnContext(ctx)
	}()
	go func() {
		cCache.CleanUpOnContext(ctx)
	}()

	setupScheduler(mgr, cCache, queues, &cfg)

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Could not run manager")
		os.Exit(1)
	}
}

func setupIndexes(ctx context.Context, mgr ctrl.Manager, cfg *config.Configuration) {
	if err := indexer.Setup(ctx, mgr.GetFieldIndexer()); err != nil {
		setupLog.Error(err, "Unable to setup core api indexes")
	}
	if isFrameworkEnabled(cfg, job.FrameworkName) {
		if err := job.SetupIndexes(ctx, mgr.GetFieldIndexer()); err != nil {
			setupLog.Error(err, "Unable to setup job indexes")
		}
	}
	if isFrameworkEnabled(cfg, mpijob.FrameworkName) {
		if err := mpijob.SetupIndexes(ctx, mgr.GetFieldIndexer()); err != nil {
			setupLog.Error(err, "Unable to setup mpijob indexes")
		}
	}
}

func setupControllers(mgr ctrl.Manager, cCache *cache.Cache, queues *queue.Manager, certsReady chan struct{}, cfg *config.Configuration) {
	// The controllers won't work until the webhooks are operating, and the webhook won't work until the
	// certs are all in place.
	setupLog.Info("Waiting for certificate generation to complete")
	<-certsReady
	setupLog.Info("Certs ready")

	if failedCtrl, err := core.SetupControllers(mgr, queues, cCache, cfg); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", failedCtrl)
		os.Exit(1)
	}
	manageJobsWithoutQueueName := cfg.ManageJobsWithoutQueueName

	if failedWebhook, err := webhooks.Setup(mgr); err != nil {
		setupLog.Error(err, "Unable to create webhook", "webhook", failedWebhook)
		os.Exit(1)
	}

	if isFrameworkEnabled(cfg, job.FrameworkName) {
		if err := job.NewReconciler(mgr.GetScheme(),
			mgr.GetClient(),
			mgr.GetEventRecorderFor(constants.JobControllerName),
			jobframework.WithManageJobsWithoutQueueName(manageJobsWithoutQueueName),
			jobframework.WithWaitForPodsReady(waitForPodsReady(cfg)),
		).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "Job")
			os.Exit(1)
		}
		if err := job.SetupWebhook(mgr, jobframework.WithManageJobsWithoutQueueName(manageJobsWithoutQueueName)); err != nil {
			setupLog.Error(err, "Unable to create webhook", "webhook", "Job")
			os.Exit(1)
		}
	} else {
		if err := noop.SetupWebhook(mgr, job.WebhookType()); err != nil {
			setupLog.Error(err, "Unable to create webhook", "webhook", "Job")
			os.Exit(1)
		}
	}

	if isFrameworkEnabled(cfg, mpijob.FrameworkName) {
		if err := mpijob.NewReconciler(mgr.GetScheme(),
			mgr.GetClient(),
			mgr.GetEventRecorderFor(constants.KueueName+"-mpijob-controller"),
			jobframework.WithManageJobsWithoutQueueName(manageJobsWithoutQueueName),
			jobframework.WithWaitForPodsReady(waitForPodsReady(cfg)),
		).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "MPIJob")
			os.Exit(1)
		}
		if err := mpijob.SetupMPIJobWebhook(mgr, jobframework.WithManageJobsWithoutQueueName(manageJobsWithoutQueueName)); err != nil {
			setupLog.Error(err, "Unable to create webhook", "webhook", "MPIJob")
			os.Exit(1)
		}
	} else {
		if err := noop.SetupWebhook(mgr, mpijob.WebhookType()); err != nil {
			setupLog.Error(err, "Unable to create webhook", "webhook", "MPIJob")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder
}

// setupProbeEndpoints registers the health endpoints
func setupProbeEndpoints(mgr ctrl.Manager) {
	defer setupLog.Info("Probe endpoints are configured on healthz and readyz")

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
}

func setupScheduler(mgr ctrl.Manager, cCache *cache.Cache, queues *queue.Manager, cfg *config.Configuration) {
	sched := scheduler.New(
		queues,
		cCache,
		mgr.GetClient(),
		mgr.GetEventRecorderFor(constants.AdmissionName),
		scheduler.WithWaitForPodsReady(waitForPodsReady(cfg)),
	)
	if err := mgr.Add(sched); err != nil {
		setupLog.Error(err, "Unable to add scheduler to manager")
		os.Exit(1)
	}
}

func waitForPodsReady(cfg *config.Configuration) bool {
	return cfg.WaitForPodsReady != nil && cfg.WaitForPodsReady.Enable
}

func encodeConfig(cfg *config.Configuration) (string, error) {
	codecs := serializer.NewCodecFactory(scheme)
	const mediaType = runtime.ContentTypeYAML
	info, ok := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), mediaType)
	if !ok {
		return "", fmt.Errorf("unable to locate encoder -- %q is not a supported media type", mediaType)
	}

	encoder := codecs.EncoderForVersion(info.Serializer, config.GroupVersion)
	buf := new(bytes.Buffer)
	if err := encoder.Encode(cfg, buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func apply(configFile string) (ctrl.Options, config.Configuration) {
	var err error
	options := ctrl.Options{
		Scheme: scheme,
	}
	cfg := config.Configuration{}

	if configFile == "" {
		scheme.Default(&cfg)
		options, err = options.AndFrom(&cfg)
	} else {
		options, err = options.AndFrom(ctrl.ConfigFile().AtPath(configFile).OfKind(&cfg))
	}
	if err != nil {
		setupLog.Error(err, "unable to load the config")
		os.Exit(1)
	}

	cfgStr, err := encodeConfig(&cfg)
	if err != nil {
		setupLog.Error(err, "unable to encode the config")
		os.Exit(1)
	}
	setupLog.Info("Successfully loaded configuration", "config", cfgStr)

	return options, cfg
}

func isFrameworkEnabled(cfg *config.Configuration, name string) bool {
	for _, framework := range cfg.Integrations.Frameworks {
		if framework == name {
			return true
		}
	}
	return false
}
