// Copyright 2023 The ingress-perf Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cloud-bulldozer/go-commons/indexers"
	"github.com/cloud-bulldozer/go-commons/prometheus"

	ocpmetadata "github.com/cloud-bulldozer/go-commons/ocp-metadata"
	"github.com/cloud-bulldozer/ingress-perf/pkg/config"
	"github.com/cloud-bulldozer/ingress-perf/pkg/runner/tools"
	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	openshiftrouteclientset "github.com/openshift/client-go/route/clientset/versioned"
	"k8s.io/client-go/tools/clientcmd"
)

var restConfig *rest.Config
var clientSet *kubernetes.Clientset
var dynamicClient *dynamic.DynamicClient
var orClientSet *openshiftrouteclientset.Clientset
var currentTuning string
var alreadyExistsNs bool

func New(uuid string, cleanup bool, opts ...OptsFunctions) *Runner {
	r := &Runner{
		uuid:    uuid,
		cleanup: cleanup,
	}
	for _, opts := range opts {
		opts(r)
	}
	return r
}

func WithIndexer(esServer, esIndex, resultsDir string, podMetrics bool) OptsFunctions {
	return func(r *Runner) {
		if esServer != "" || resultsDir != "" {
			var indexerCfg indexers.IndexerConfig
			if esServer != "" {
				indexerCfg = indexers.IndexerConfig{
					Type:    indexers.ElasticIndexer,
					Servers: []string{esServer},
					Index:   esIndex,
				}
			} else if resultsDir != "" {
				indexerCfg = indexers.IndexerConfig{
					Type:             indexers.LocalIndexer,
					MetricsDirectory: resultsDir,
				}
			}
			log.Infof("Creating %s indexer", indexerCfg.Type)
			indexer, err := indexers.NewIndexer(indexerCfg)
			if err != nil {
				log.Fatal(err)
			}
			r.indexer = indexer
			r.podMetrics = podMetrics
		}
	}
}

func WithNamespace(namespace string) OptsFunctions {
	return func(r *Runner) {
		if errs := validation.IsDNS1123Subdomain(namespace); errs != nil {
			log.Fatalf("Invalid namespace: %v", errs)
		}
		benchmarkNs = namespace
	}
}

func (r *Runner) Start() error {
	var err error
	var kubeconfig string
	var benchmarkResult []tools.Result
	var clusterMetadata tools.ClusterMetadata
	var benchmarkResultDocuments []interface{}
	passed := true
	if os.Getenv("KUBECONFIG") != "" {
		kubeconfig = os.Getenv("KUBECONFIG")
	} else if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".kube", "config")); kubeconfig == "" && !os.IsNotExist(err) {
		kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
	}
	restConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return err
	}
	restConfig.QPS = 200
	restConfig.Burst = 200
	clientSet = kubernetes.NewForConfigOrDie(restConfig)
	orClientSet = openshiftrouteclientset.NewForConfigOrDie(restConfig)
	dynamicClient = dynamic.NewForConfigOrDie(restConfig)
	ocpMetadata, err := ocpmetadata.NewMetadata(restConfig)
	if err != nil {
		return err
	}
	clusterMetadata.ClusterMetadata, err = ocpMetadata.GetClusterMetadata()
	if err != nil {
		return err
	}
	promURL, promToken, err := ocpMetadata.GetPrometheus()
	if err != nil {
		log.Error("Error fetching prometheus information")
		return err
	}
	p, err := prometheus.NewClient(promURL, promToken, "", "", true)
	if err != nil {
		log.Error("Error creating prometheus client")
		return err
	}
	clusterMetadata.HAProxyVersion, err = getHAProxyVersion()
	if err != nil {
		log.Errorf("Couldn't fetch haproxy version: %v", err)
	} else {
		log.Infof("HAProxy version: %s", clusterMetadata.HAProxyVersion)
	}
	if err := deployAssets(); err != nil {
		return err
	}
	for i, cfg := range config.Cfg {
		cfg.UUID = r.uuid
		log.Infof("Running test %d/%d", i+1, len(config.Cfg))
		log.Infof("Tool:%s termination:%v servers:%d concurrency:%d procs:%d connections:%d duration:%v",
			cfg.Tool,
			cfg.Termination,
			cfg.ServerReplicas,
			cfg.Concurrency,
			cfg.Procs,
			cfg.Connections,
			cfg.Duration,
		)
		if err := reconcileNs(cfg); err != nil {
			return err
		}
		if cfg.Tuning != "" {
			currentTuning = cfg.Tuning
			if err = applyTunning(cfg.Tuning); err != nil {
				return err
			}
		}
		if benchmarkResult, err = runBenchmark(cfg, clusterMetadata, p, r.podMetrics); err != nil {
			return err
		}
		if r.indexer != nil && !cfg.Warmup {
			for _, res := range benchmarkResult {
				benchmarkResultDocuments = append(benchmarkResultDocuments, res)
			}
			// When not using local indexer, empty the documents array when all documents after indexing them
			if _, ok := (*r.indexer).(*indexers.Local); !ok {
				if indexDocuments(*r.indexer, benchmarkResultDocuments, indexers.IndexingOpts{}) != nil {
					log.Errorf("Indexing error: %v", err.Error())
				}
				benchmarkResultDocuments = []interface{}{}
			}
		}
	}
	if _, ok := (*r.indexer).(*indexers.Local); r.indexer != nil && ok {
		if err := indexDocuments(*r.indexer, benchmarkResultDocuments, indexers.IndexingOpts{MetricName: r.uuid}); err != nil {
			log.Errorf("Indexing error: %v", err.Error())
		}
	}
	if r.cleanup {
		if cleanup(10*time.Minute) != nil {
			return err
		}
	}
	if passed {
		return nil
	}
	return fmt.Errorf("some benchmark comparisons failed")
}

func indexDocuments(indexer indexers.Indexer, documents []interface{}, indexingOpts indexers.IndexingOpts) error {
	msg, err := indexer.Index(documents, indexingOpts)
	if err != nil {
		return err
	}
	log.Info(msg)
	return nil
}

func cleanup(timeout time.Duration) error {
	log.Info("Cleaning up resources")
	if alreadyExistsNs {
		if err := clientSet.AppsV1().Deployments(benchmarkNs).Delete(context.TODO(), client.Name, metav1.DeleteOptions{}); err != nil {
			return err
		}
		if err := clientSet.AppsV1().Deployments(benchmarkNs).Delete(context.TODO(), server.Name, metav1.DeleteOptions{}); err != nil {
			return err
		}
		if err := clientSet.CoreV1().Services(benchmarkNs).Delete(context.TODO(), service.Name, metav1.DeleteOptions{}); err != nil {
			return err
		}
		for _, route := range routes {
			if err := orClientSet.RouteV1().Routes(benchmarkNs).Delete(context.TODO(), route.Name, metav1.DeleteOptions{}); err != nil {
				return err
			}
		}
	} else {
		if err := clientSet.CoreV1().Namespaces().Delete(context.TODO(), benchmarkNs, metav1.DeleteOptions{}); err != nil {
			return err
		}
		err := wait.PollUntilContextTimeout(context.TODO(), time.Second, timeout, true, func(ctx context.Context) (bool, error) {
			_, err := clientSet.CoreV1().Namespaces().Get(context.TODO(), benchmarkNs, metav1.GetOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					return true, nil
				}
				return false, err
			}
			return false, nil
		})
		if err != nil {
			return err
		}
	}
	return clientSet.RbacV1().ClusterRoleBindings().Delete(context.Background(), getClientCRB(benchmarkNs).Name, metav1.DeleteOptions{})
}

func deployAssets() error {
	log.Infof("Deploying benchmark assets")
	_, err := clientSet.CoreV1().Namespaces().Create(context.TODO(), getNamespace(benchmarkNs), metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			alreadyExistsNs = true
		} else {
			return err
		}
	}
	_, err = clientSet.AppsV1().Deployments(benchmarkNs).Create(context.TODO(), &server, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	_, err = clientSet.RbacV1().ClusterRoleBindings().Create(context.TODO(), getClientCRB(benchmarkNs), metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	_, err = clientSet.AppsV1().Deployments(benchmarkNs).Create(context.TODO(), &client, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	_, err = clientSet.CoreV1().Services(benchmarkNs).Create(context.TODO(), &service, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	for _, route := range routes {
		_, err = orClientSet.RouteV1().Routes(benchmarkNs).Create(context.TODO(), &route, metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func reconcileNs(cfg config.Config) error {
	f := func(deployment appsv1.Deployment, replicas int32) error {
		d, err := clientSet.AppsV1().Deployments(benchmarkNs).Get(context.TODO(), deployment.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if d.Status.ReadyReplicas == replicas {
			return nil
		}
		deployment.Spec.Replicas = &replicas
		_, err = clientSet.AppsV1().Deployments(benchmarkNs).Update(context.TODO(), &deployment, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		return waitForDeployment(benchmarkNs, deployment.Name, time.Minute)
	}
	if err := f(server, cfg.ServerReplicas); err != nil {
		return err
	}
	return f(client, cfg.Concurrency)
}

func waitForDeployment(ns, deployment string, maxWaitTimeout time.Duration) error {
	var errMsg string
	var dep *appsv1.Deployment
	var err error
	log.Infof("Waiting for replicas from deployment %s in ns %s to be ready", deployment, ns)
	err = wait.PollUntilContextTimeout(context.TODO(), time.Second, maxWaitTimeout, true, func(ctx context.Context) (bool, error) {
		dep, err = clientSet.AppsV1().Deployments(ns).Get(context.TODO(), deployment, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if *dep.Spec.Replicas != dep.Status.ReadyReplicas || *dep.Spec.Replicas != dep.Status.AvailableReplicas {
			errMsg = fmt.Sprintf("%d/%d replicas ready", dep.Status.AvailableReplicas, *dep.Spec.Replicas)
			log.Debug(errMsg)
			return false, nil
		}
		log.Debugf("%d replicas from deployment %s ready", dep.Status.UpdatedReplicas, deployment)
		return true, nil
	})
	if err != nil && errMsg != "" {
		log.Error(errMsg)
		failedPods, _ := clientSet.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{
			FieldSelector: "status.phase=Pending",
			LabelSelector: labels.SelectorFromSet(dep.Spec.Selector.MatchLabels).String(),
		})
		for _, pod := range failedPods.Items {
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.State.Waiting != nil {
					log.Errorf("%v@%v: %v", pod.Name, pod.Spec.NodeName, cs.State.Waiting.Message)
				}
			}
		}
	}
	return err
}
