/*
Copyright 2022 The Crossplane Authors.

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

package cluster

import (
	"bytes"
	"context"
	"fmt"
	"text/template"
	"time"

	"github.com/Masterminds/sprig/v3"
	cosiresource "github.com/cosi-project/runtime/pkg/resource"
	cosistate "github.com/cosi-project/runtime/pkg/state"
	"github.com/pkg/errors"
	omniclient "github.com/siderolabs/omni/client/pkg/client"
	"github.com/siderolabs/omni/client/pkg/client/management"
	"github.com/siderolabs/omni/client/pkg/omni/resources"
	omnitemplate "github.com/siderolabs/omni/client/pkg/template"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/connection"
	"github.com/crossplane/crossplane-runtime/pkg/controller"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	"github.com/trevex/provider-omni/apis/cluster/v1alpha1"
	apisv1alpha1 "github.com/trevex/provider-omni/apis/v1alpha1"
	"github.com/trevex/provider-omni/internal/features"
)

const (
	errNotCluster   = "managed resource is not a Cluster custom resource"
	errTrackPCUsage = "cannot track ProviderConfig usage"
	errGetPC        = "cannot get ProviderConfig"
	errGetCreds     = "cannot get credentials"

	errNewClient = "cannot create new Service"

	clusterTemplateRaw = `
kind: Cluster
name: {{.Name}}
kubernetes:
  version: {{.KubernetesVersion}}
talos:
  version: {{.TalosVersion}}
{{- if .ConfigPatch }}
patches:
  - name: default
    inline:
	  {{- .ConfigPatch | nindent 6 }}
{{- end }}
---
kind: ControlPlane
machineClass:
  name: {{.ControlPlane.MachineClass.Name}}
  size: {{.ControlPlane.MachineClass.Size}}
{{- range $i, $w := .Workers }}
---
kind: Workers
name: {{$w.Name}}
machineClass:
  name: {{$w.MachineClass.Name}}
  size: {{$w.MachineClass.Size}}
{{- end }}
`
)

var (
	clusterTemplate = template.Must(
		template.New("cluster").Funcs(sprig.FuncMap()).Parse(clusterTemplateRaw),
	)
)

// Setup adds a controller that reconciles Cluster managed resources.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := managed.ControllerName(v1alpha1.ClusterGroupKind)

	cps := []managed.ConnectionPublisher{managed.NewAPISecretPublisher(mgr.GetClient(), mgr.GetScheme())}
	if o.Features.Enabled(features.EnableAlphaExternalSecretStores) {
		cps = append(cps, connection.NewDetailsManager(mgr.GetClient(), apisv1alpha1.StoreConfigGroupVersionKind))
	}

	r := managed.NewReconciler(mgr,
		resource.ManagedKind(v1alpha1.ClusterGroupVersionKind),
		managed.WithExternalConnecter(&connector{
			kube:   mgr.GetClient(),
			usage:  resource.NewProviderConfigUsageTracker(mgr.GetClient(), &apisv1alpha1.ProviderConfigUsage{}),
			logger: o.Logger,
		}),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithPollInterval(o.PollInterval),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))),
		managed.WithConnectionPublishers(cps...))

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		WithEventFilter(resource.DesiredStateChanged()).
		For(&v1alpha1.Cluster{}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
}

// A connector is expected to produce an ExternalClient when its Connect method
// is called.
type connector struct {
	kube   client.Client
	usage  resource.Tracker
	logger logging.Logger
}

// Connect typically produces an ExternalClient by:
// 1. Tracking that the managed resource is using a ProviderConfig.
// 2. Getting the managed resource's ProviderConfig.
// 3. Getting the credentials specified by the ProviderConfig.
// 4. Using the credentials to form a client.
func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	cr, ok := mg.(*v1alpha1.Cluster)
	if !ok {
		return nil, errors.New(errNotCluster)
	}

	if err := c.usage.Track(ctx, mg); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	pc := &apisv1alpha1.ProviderConfig{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: cr.GetProviderConfigReference().Name}, pc); err != nil {
		return nil, errors.Wrap(err, errGetPC)
	}

	cd := pc.Spec.Credentials
	data, err := resource.CommonCredentialExtractor(ctx, cd.Source, c.kube, cd.CommonCredentialSelectors)
	if err != nil {
		return nil, errors.Wrap(err, errGetCreds)
	}

	client, err := omniclient.New(pc.Spec.Endpoint, omniclient.WithServiceAccount(
		string(data),
	))
	if err != nil {
		return nil, errors.Wrap(err, errNewClient)
	}

	return &external{client: client, localKube: c.kube, logger: c.logger}, nil
}

// An ExternalClient observes, then either creates, updates, or deletes an
// external resource to ensure it reflects the managed resource's desired state.
type external struct {
	client    *omniclient.Client
	localKube client.Client
	logger    logging.Logger
}

func (c *external) Disconnect(ctx context.Context) error {
	return c.client.Close()
}

//nolint:gocognit,gocyclo,cyclop
func (c *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.Cluster)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotCluster)
	}

	if meta.WasDeleted(cr) {
		destroyLen, err := c.getObservedDestroyLen(ctx, cr)
		if err != nil {
			return managed.ExternalObservation{}, errors.Wrap(err, "failed to calculate observed length of destroyed resources")
		}
		if destroyLen > 0 {
			return managed.ExternalObservation{ResourceExists: true}, nil
		}
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	syncResult, err := c.syncResultForCR(ctx, cr)
	if err != nil {
		return managed.ExternalObservation{}, errors.Wrap(err, "failed to sync")
	}

	c.logger.Debug("observe cluster", "syncResult", syncResult)
	destroyLen := 0
	for _, phase := range syncResult.Destroy {
		destroyLen += len(phase)
	}

	if len(syncResult.Create) != 0 && len(syncResult.Update) == 0 && destroyLen == 0 {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	if len(syncResult.Create) > 0 || len(syncResult.Update) > 0 || destroyLen > 0 {
		return managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: false}, nil
	}

	cd := managed.ConnectionDetails{}
	if cr.Status.AtProvider.KubeconfigExpiry == nil || cr.Status.AtProvider.KubeconfigExpiry.Time.Before(time.Now()) {
		ttl, _ := time.ParseDuration("8760h0m0s")
		data, err := c.client.Management().
			WithCluster(cr.Spec.ForProvider.Name).
			Kubeconfig(ctx, management.WithServiceAccount(
				ttl,
				"admin",
				"system:masters",
			))
		if err != nil {
			return managed.ExternalObservation{}, errors.Wrap(err, "failed to fetch kubeconfig")
		}
		expiry := metav1.NewTime(time.Now().Add(ttl - time.Hour))
		cr.Status.AtProvider.KubeconfigExpiry = &expiry
		c.logger.Debug("connection details", "kubeconfig", string(data), "error", err)
		cd["kubeconfig"] = data
	}

	cr.Status.SetConditions(xpv1.Available())

	// syncResult all equal to 0
	return managed.ExternalObservation{
		ResourceExists:    true,
		ResourceUpToDate:  true,
		ConnectionDetails: cd,
	}, nil
}

func (c *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	cr, ok := mg.(*v1alpha1.Cluster)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotCluster)
	}

	syncResult, err := c.syncResultForCR(ctx, cr)
	if err != nil {
		return managed.ExternalCreation{}, errors.Wrap(err, "failed to sync")
	}

	st := c.client.Omni().State()
	for _, r := range syncResult.Create {
		if err = st.Create(ctx, r); err != nil {
			return managed.ExternalCreation{}, errors.Wrap(err, "failed to create")
		}
	}

	return managed.ExternalCreation{}, err
}

func (c *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	cr, ok := mg.(*v1alpha1.Cluster)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errNotCluster)
	}

	syncResult, err := c.syncResultForCR(ctx, cr)
	if err != nil {
		return managed.ExternalUpdate{}, errors.Wrap(err, "failed to sync")
	}

	st := c.client.Omni().State()
	for _, r := range syncResult.Create {
		if err = st.Create(ctx, r); err != nil {
			return managed.ExternalUpdate{}, errors.Wrap(err, "failed to create")
		}
	}
	for _, p := range syncResult.Update {
		if err = st.Update(ctx, p.New); err != nil {
			return managed.ExternalUpdate{}, errors.Wrap(err, "failed to update")
		}
	}
	for _, phase := range syncResult.Destroy {
		if err := c.syncDeleteResources(ctx, phase); err != nil {
			return managed.ExternalUpdate{}, errors.Wrap(err, "failed to destroy")
		}
	}

	return managed.ExternalUpdate{}, err
}

func (c *external) Delete(ctx context.Context, mg resource.Managed) (managed.ExternalDelete, error) {
	cr, ok := mg.(*v1alpha1.Cluster)
	if !ok {
		return managed.ExternalDelete{}, errors.New(errNotCluster)
	}

	st := c.client.Omni().State()

	tmpl, err := c.loadTemplateForCR(cr)
	if err != nil {
		return managed.ExternalDelete{}, err
	}

	syncResult, err := tmpl.Delete(ctx, st)
	if err != nil {
		return managed.ExternalDelete{}, errors.Wrap(err, "failed to get SyncResult for delete")
	}

	for _, phase := range syncResult.Destroy {
		if err := c.syncDeleteResources(ctx, phase); err != nil {
			return managed.ExternalDelete{}, errors.Wrap(err, "failed to destroy")
		}
	}

	c.logger.Debug("DELETION SUCCESSFUL!")

	return managed.ExternalDelete{}, nil
}

func (c *external) getObservedDestroyLen(ctx context.Context, cr *v1alpha1.Cluster) (int, error) {
	st := c.client.Omni().State()

	tmpl, err := c.loadTemplateForCR(cr)
	if err != nil {
		return -1, err
	}

	syncResult, err := tmpl.Delete(ctx, st)
	if err != nil {
		return -1, errors.Wrap(err, "failed to get SyncResult for delete")
	}

	destroyLen := 0
	for _, phase := range syncResult.Destroy {
		destroyLen += len(phase)
	}
	return destroyLen, nil
}

func (c *external) loadTemplateForCR(cr *v1alpha1.Cluster) (*omnitemplate.Template, error) {
	var buf bytes.Buffer
	err := clusterTemplate.Execute(&buf, cr.Spec.ForProvider)
	if err != nil {
		return nil, errors.Wrap(err, "failed to execute go template")
	}

	tmpl, err := omnitemplate.Load(&buf)
	if err != nil {
		return nil, errors.Wrap(err, "failed to load template")
	}

	if err = tmpl.Validate(); err != nil {
		return nil, errors.Wrap(err, "failed to validate template")
	}

	return tmpl, nil
}

func (c *external) syncResultForCR(ctx context.Context, cr *v1alpha1.Cluster) (*omnitemplate.SyncResult, error) {
	st := c.client.Omni().State()

	tmpl, err := c.loadTemplateForCR(cr)
	if err != nil {
		return nil, err
	}

	return tmpl.Sync(ctx, st)
}

// https://github.com/siderolabs/omni/blob/main/client/pkg/template/operations/sync.go#L115
//
//nolint:gocognit,gocyclo,cyclop
func (c *external) syncDeleteResources(ctx context.Context, toDelete []cosiresource.Resource) error {
	st := c.client.Omni().State()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	teardownWatch := make(chan cosistate.Event)
	tearingDownResourceTypes := map[cosiresource.Type]struct{}{}

	for _, r := range toDelete {
		tearingDownResourceTypes[r.Metadata().Type()] = struct{}{}
	}

	for resourceType := range tearingDownResourceTypes {
		if err := st.WatchKind(ctx, cosiresource.NewMetadata(resources.DefaultNamespace, resourceType, "", cosiresource.VersionUndefined), teardownWatch, cosistate.WithBootstrapContents(true)); err != nil {
			return err
		}
	}

	tearingDownResources := map[string]struct{}{}

	for _, r := range toDelete {
		if _, err := st.Teardown(ctx, r.Metadata()); err != nil && !cosistate.IsNotFoundError(err) {
			return err
		}

		tearingDownResources[describe(r)] = struct{}{}
	}

	for len(tearingDownResources) > 0 {
		var event cosistate.Event

		select {
		case <-ctx.Done():
			return ctx.Err()
		case event = <-teardownWatch:
		}

		switch event.Type {
		case cosistate.Updated, cosistate.Created:
			if _, ok := tearingDownResources[describe(event.Resource)]; ok {
				if event.Resource.Metadata().Phase() == cosiresource.PhaseTearingDown && event.Resource.Metadata().Finalizers().Empty() {
					if err := st.Destroy(ctx, event.Resource.Metadata()); err != nil && !cosistate.IsNotFoundError(err) {
						return err
					}
				}
			}
		case cosistate.Destroyed:
			delete(tearingDownResources, describe(event.Resource))
		case cosistate.Bootstrapped:
			// ignore
		case cosistate.Errored:
			return event.Error
		}
	}

	return nil
}

// https://github.com/siderolabs/omni/blob/main/client/pkg/template/operations/internal/utils/utils.go
func describe(r cosiresource.Resource) string {
	return fmt.Sprintf("%s(%s)", r.Metadata().Type(), r.Metadata().ID())
}
