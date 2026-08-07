/*
Copyright 2026.

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

package controller

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/storage/driver"
	"sigs.k8s.io/yaml"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	memcached "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	sdcorev1alpha1 "github.com/your-org/sdcore-operator/api/v1alpha1"
)

// charts/<component> directories are baked into the operator binary.
//
//go:embed all:charts
var chartsFS embed.FS

// componentToChart maps the CR's spec.component to an embedded chart dir.
var componentToChart = map[string]string{
	"controlplane": "charts/controlplane",
	"upf":          "charts/upf",
	"ransim":       "charts/ransim",
	"subscribers":  "charts/subscribers",
}

// SDCoreDeploymentReconciler reconciles a SDCoreDeployment object
type SDCoreDeploymentReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	RestConfig *rest.Config
}

// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=sdcoredeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=sdcoredeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sdcore.nephio.io,resources=sdcoredeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups="*",resources="*",verbs=get;list;watch;create;update;patch;delete

func (r *SDCoreDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cr sdcorev1alpha1.SDCoreDeployment
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ns := cr.Spec.TargetNamespace
	if ns == "" {
		ns = cr.Namespace
	}

	ch, err := loadChart(cr.Spec.Component)
	if err != nil {
		return r.fail(ctx, &cr, fmt.Sprintf("load chart: %v", err))
	}

	vals := map[string]interface{}{}
	if cr.Spec.Values != nil && len(cr.Spec.Values.Raw) > 0 {
		if err := yaml.Unmarshal(cr.Spec.Values.Raw, &vals); err != nil {
			return r.fail(ctx, &cr, fmt.Sprintf("parse values: %v", err))
		}
	}

	cfg := new(action.Configuration)
	getter := newRESTClientGetter(r.RestConfig, ns)
	if err := cfg.Init(getter, ns, "secret", func(format string, a ...interface{}) {
		log.Info(fmt.Sprintf(format, a...))
	}); err != nil {
		return r.fail(ctx, &cr, fmt.Sprintf("helm init: %v", err))
	}

	relName := cr.Name
	var revision int

	hist := action.NewHistory(cfg)
	hist.Max = 1
	if _, herr := hist.Run(relName); herr == driver.ErrReleaseNotFound {
		inst := action.NewInstall(cfg)
		inst.ReleaseName = relName
		inst.Namespace = ns
		inst.CreateNamespace = true
		inst.Timeout = 5 * time.Minute
		rel, ierr := inst.Run(ch, vals)
		if ierr != nil {
			return r.fail(ctx, &cr, fmt.Sprintf("helm install: %v", ierr))
		}
		revision = rel.Version
		log.Info("installed release", "name", relName, "revision", revision)
	} else if herr != nil {
		return r.fail(ctx, &cr, fmt.Sprintf("helm history: %v", herr))
	} else {
		upg := action.NewUpgrade(cfg)
		upg.Namespace = ns
		upg.Timeout = 5 * time.Minute
		rel, uerr := upg.Run(relName, ch, vals)
		if uerr != nil {
			return r.fail(ctx, &cr, fmt.Sprintf("helm upgrade: %v", uerr))
		}
		revision = rel.Version
		log.Info("upgraded release", "name", relName, "revision", revision)
	}

	cr.Status.Phase = "Deployed"
	cr.Status.Message = fmt.Sprintf("helm release %q revision %d applied to %q", relName, revision, ns)
	cr.Status.ReleaseRevision = revision
	cr.Status.ObservedGeneration = cr.Generation
	apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type:    "Available",
		Status:  metav1.ConditionTrue,
		Reason:  "Deployed",
		Message: cr.Status.Message,
	})
	if err := r.Status().Update(ctx, &cr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *SDCoreDeploymentReconciler) fail(ctx context.Context, cr *sdcorev1alpha1.SDCoreDeployment, msg string) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Error(fmt.Errorf(msg), "reconcile failed")
	cr.Status.Phase = "Failed"
	cr.Status.Message = msg
	cr.Status.ObservedGeneration = cr.Generation
	apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type:    "Available",
		Status:  metav1.ConditionFalse,
		Reason:  "Error",
		Message: msg,
	})
	_ = r.Status().Update(ctx, cr)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// loadChart reads an embedded chart directory into a *chart.Chart.
func loadChart(component string) (*chart.Chart, error) {
	dir, ok := componentToChart[component]
	if !ok {
		return nil, fmt.Errorf("unknown component %q", component)
	}
	var files []*loader.BufferedFile
	err := fs.WalkDir(chartsFS, dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, rerr := chartsFS.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		// helm wants paths relative to the chart root
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		files = append(files, &loader.BufferedFile{Name: filepath.ToSlash(rel), Data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files found under %q (chart not embedded?)", dir)
	}
	return loader.LoadFiles(files)
}

// SetupWithManager sets up the controller with the Manager.
func (r *SDCoreDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RestConfig == nil {
		r.RestConfig = mgr.GetConfig()
	}
	return ctrl.NewControllerManagedBy(mgr).
		// only reconcile on SPEC changes, not our own status writes -> no helm-upgrade loop
		For(&sdcorev1alpha1.SDCoreDeployment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("sdcoredeployment").
		Complete(r)
}

// ---------- minimal RESTClientGetter for the Helm action.Configuration ----------

type restClientGetter struct {
	restConfig *rest.Config
	namespace  string
}

func newRESTClientGetter(cfg *rest.Config, namespace string) *restClientGetter {
	return &restClientGetter{restConfig: rest.CopyConfig(cfg), namespace: namespace}
}

func (g *restClientGetter) ToRESTConfig() (*rest.Config, error) {
	return g.restConfig, nil
}

func (g *restClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(g.restConfig)
	if err != nil {
		return nil, err
	}
	return memcached.NewMemCacheClient(dc), nil
}

func (g *restClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	dc, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(dc), nil
}

func (g *restClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	overrides := &clientcmd.ConfigOverrides{Context: clientcmdapi.Context{Namespace: g.namespace}}
	return clientcmd.NewDefaultClientConfig(*clientcmdapi.NewConfig(), overrides)
}

