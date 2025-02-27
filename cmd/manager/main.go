/*
Copyright 2024.

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
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"github.com/kaasops/vector-operator/internal/buildinfo"
	"os"
	"time"

	monitorv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/kaasops/vector-operator/internal/utils/k8s"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/kaasops/vector-operator/api/v1alpha1"
	"github.com/kaasops/vector-operator/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	utilruntime.Must(monitorv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	var watchNamespace string
	var watchLabel string
	var configCheckTimeout time.Duration

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&watchNamespace, "watch-namespace", "", "Namespace to filter the list of watched objects")
	flag.StringVar(&watchLabel, "watch-name", "", "Filter the list of watched objects by checking the app.kubernetes.io/managed-by label")
	flag.DurationVar(&configCheckTimeout, "configcheck-timeout", 300*time.Second, "configcheck timeout")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog.Info("build info", "version", buildinfo.Version)

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: tlsOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		// TODO(user): TLSOpts is used to allow configuring the TLS config used for the server. If certificates are
		// not provided, self-signed certificates will be generated by default. This option is not recommended for
		// production environments as self-signed certificates do not offer the same level of trust and security
		// as certificates issued by a trusted Certificate Authority (CA). The primary risk is potentially allowing
		// unauthorized access to sensitive metrics data. Consider replacing with CertDir, CertName, and KeyName
		// to provide certificates, ensuring the server communicates using trusted and secure certificates.
		TLSOpts: tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	config := ctrl.GetConfigOrDie()

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		setupLog.Error(err, "unable to create clientset")
		os.Exit(1)
	}

	dc, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		setupLog.Error(err, "unable to create discovery client")
		os.Exit(1)
	}

	mgrOptions := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "79cbe7f3.kaasops.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	}
	customMgrOptions, err := setupCustomCache(&mgrOptions, watchNamespace, watchLabel)
	if err != nil {
		setupLog.Error(err, "unable to set up custom cache settings")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(config, *customMgrOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	vectorAgentEventCh := make(chan event.GenericEvent)
	defer close(vectorAgentEventCh)

	if err = (&controller.VectorReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		Clientset:          clientset,
		ConfigCheckTimeout: configCheckTimeout,
		DiscoveryClient:    dc,
		EventChan:          vectorAgentEventCh,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Vector")
		os.Exit(1)
	}

	vectorAgentsPipelineEventCh := make(chan event.GenericEvent, 10)
	defer close(vectorAgentsPipelineEventCh)
	vectorAggregatorsPipelineEventCh := make(chan event.GenericEvent, 10)
	defer close(vectorAggregatorsPipelineEventCh)
	clusterVectorAggregatorsPipelineEventCh := make(chan event.GenericEvent, 10)
	defer close(clusterVectorAggregatorsPipelineEventCh)

	if err = (&controller.PipelineReconciler{
		Client:                          mgr.GetClient(),
		Scheme:                          mgr.GetScheme(),
		Clientset:                       clientset,
		ConfigCheckTimeout:              configCheckTimeout,
		VectorAgentEventCh:              vectorAgentsPipelineEventCh,
		VectorAggregatorsEventCh:        vectorAggregatorsPipelineEventCh,
		ClusterVectorAggregatorsEventCh: clusterVectorAggregatorsPipelineEventCh,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VectorPipeline")
		os.Exit(1)
	}

	vectorAggregatorsEventCh := make(chan event.GenericEvent)
	defer close(vectorAggregatorsEventCh)

	if err = (&controller.VectorAggregatorReconciler{
		Client:             mgr.GetClient(),
		Clientset:          clientset,
		Scheme:             mgr.GetScheme(),
		ConfigCheckTimeout: configCheckTimeout,
		EventChan:          vectorAggregatorsEventCh,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VectorAggregator")
		os.Exit(1)
	}

	clusterVectorAggregatorsEventCh := make(chan event.GenericEvent)
	defer close(clusterVectorAggregatorsEventCh)

	if err = (&controller.ClusterVectorAggregatorReconciler{
		Client:             mgr.GetClient(),
		Clientset:          clientset,
		Scheme:             mgr.GetScheme(),
		ConfigCheckTimeout: configCheckTimeout,
		EventChan:          clusterVectorAggregatorsEventCh,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClusterVectorAggregator")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	go reconcileWithDelay(context.Background(), vectorAgentsPipelineEventCh, vectorAgentEventCh, time.Second*10)
	go reconcileWithDelay(context.Background(), vectorAggregatorsPipelineEventCh, vectorAggregatorsEventCh, time.Second*10)
	go reconcileWithDelay(context.Background(), clusterVectorAggregatorsPipelineEventCh, clusterVectorAggregatorsEventCh, time.Second*10)

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func setupCustomCache(mgrOptions *ctrl.Options, namespace string, watchLabel string) (*ctrl.Options, error) {
	if namespace == "" && watchLabel == "" {
		return mgrOptions, nil
	}

	if namespace == "" {
		namespace = cache.AllNamespaces
	}

	var labelSelector labels.Selector
	if watchLabel != "" {
		labelSelector = labels.Set{k8s.ManagedByLabelKey: "vector-operator", k8s.NameLabelKey: watchLabel}.AsSelector()
	} else {
		labelSelector = labels.Everything()
	}

	mgrOptions.Cache = cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}: {
				Namespaces: map[string]cache.Config{
					namespace: {
						LabelSelector: labelSelector,
					},
				},
			},
			&appsv1.DaemonSet{}: {
				Namespaces: map[string]cache.Config{
					namespace: {
						LabelSelector: labelSelector,
					},
				},
			},
			&corev1.Service{}: {
				Namespaces: map[string]cache.Config{
					namespace: {
						LabelSelector: labelSelector,
					},
				},
			},
			&corev1.Secret{}: {
				Namespaces: map[string]cache.Config{
					namespace: {
						LabelSelector: labelSelector,
					},
				},
			},
			&corev1.ServiceAccount{}: {
				Namespaces: map[string]cache.Config{
					namespace: {
						LabelSelector: labelSelector,
					},
				},
			},
		},
	}

	return mgrOptions, nil
}

func reconcileWithDelay(ctx context.Context, in, out chan event.GenericEvent, delay time.Duration) {
	ticker := time.NewTicker(delay)
	defer ticker.Stop()

	store := make(map[string]event.GenericEvent)

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-in:
			ticker.Reset(delay)
			key := fmt.Sprintf("%s/%s", ev.Object.GetNamespace(), ev.Object.GetName())
			if _, ok := store[key]; !ok {
				store[key] = ev
			}
		case <-ticker.C:
			if len(store) != 0 {
				for nn, ev := range store {
					out <- ev
					delete(store, nn)
				}
			}
		}
	}
}
