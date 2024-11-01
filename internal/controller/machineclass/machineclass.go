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

package machineclass

import (
	"context"
	"fmt"

	cosiresource "github.com/cosi-project/runtime/pkg/resource"
	"github.com/pkg/errors"
	"github.com/siderolabs/omni/client/api/omni/specs"
	omniclient "github.com/siderolabs/omni/client/pkg/client"
	"github.com/siderolabs/omni/client/pkg/omni/resources"
	resourcesomni "github.com/siderolabs/omni/client/pkg/omni/resources/omni"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/connection"
	"github.com/crossplane/crossplane-runtime/pkg/controller"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	"github.com/trevex/provider-omni/apis/machine/v1alpha1"
	apisv1alpha1 "github.com/trevex/provider-omni/apis/v1alpha1"
	"github.com/trevex/provider-omni/internal/features"
)

const (
	errNotMachineClass = "managed resource is not a MachineClass custom resource"
	errTrackPCUsage    = "cannot track ProviderConfig usage"
	errGetPC           = "cannot get ProviderConfig"
	errGetCreds        = "cannot get credentials"

	errNewClient = "cannot create new Service"

	// TODO: move static string error message up here...
)

// Setup adds a controller that reconciles MachineClass managed resources.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := managed.ControllerName(v1alpha1.MachineClassGroupKind)

	cps := []managed.ConnectionPublisher{managed.NewAPISecretPublisher(mgr.GetClient(), mgr.GetScheme())}
	if o.Features.Enabled(features.EnableAlphaExternalSecretStores) {
		cps = append(cps, connection.NewDetailsManager(mgr.GetClient(), apisv1alpha1.StoreConfigGroupVersionKind))
	}

	r := managed.NewReconciler(mgr,
		resource.ManagedKind(v1alpha1.MachineClassGroupVersionKind),
		managed.WithExternalConnecter(&connector{
			kube:  mgr.GetClient(),
			usage: resource.NewProviderConfigUsageTracker(mgr.GetClient(), &apisv1alpha1.ProviderConfigUsage{}),
		}),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithPollInterval(o.PollInterval),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))),
		managed.WithConnectionPublishers(cps...))

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		WithEventFilter(resource.DesiredStateChanged()).
		For(&v1alpha1.MachineClass{}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
}

// A connector is expected to produce an ExternalClient when its Connect method
// is called.
type connector struct {
	kube  client.Client
	usage resource.Tracker
}

// Connect typically produces an ExternalClient by:
// 1. Tracking that the managed resource is using a ProviderConfig.
// 2. Getting the managed resource's ProviderConfig.
// 3. Getting the credentials specified by the ProviderConfig.
// 4. Using the credentials to form a client.
func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	cr, ok := mg.(*v1alpha1.MachineClass)
	if !ok {
		return nil, errors.New(errNotMachineClass)
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

	return &external{client: client, localKube: c.kube}, nil
}

// An ExternalClient observes, then either creates, updates, or deletes an
// external resource to ensure it reflects the managed resource's desired state.
type external struct {
	client    *omniclient.Client
	localKube client.Client
}

func (c *external) Disconnect(ctx context.Context) error {
	return c.client.Close()
}

func (c *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.MachineClass)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotMachineClass)
	}

	st := c.client.Omni().State()
	res, err := st.Get(ctx, pointerForCR(cr))
	if err != nil {
		//lint:ignore nilerr we intentionally ignore the error as it indicates resource not found
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	actualMC, ok := res.(*resourcesomni.MachineClass)
	if !ok {
		return managed.ExternalObservation{}, errors.New("resource is not a machineclass")
	}

	expectedMC := machineClassForCR(cr)
	// Let's copy over metadata to minimize the diff
	err = syncCOSIMetadata(expectedMC, actualMC)
	if err != nil {
		return managed.ExternalObservation{}, errors.Wrap(err, "failed to sync metadata")
	}

	// These fmt statements should be removed in the real implementation.
	fmt.Printf("Observing: %+v", cr)
	fmt.Printf("  Actual: %+v", actualMC)
	fmt.Printf("  Expected: %+v", expectedMC)

	if !cosiresource.Equal(actualMC, expectedMC) {
		return managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: false}, nil
	}

	cr.Status.SetConditions(xpv1.Available())

	return managed.ExternalObservation{
		ResourceExists:    true,
		ResourceUpToDate:  true,
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

func (c *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	cr, ok := mg.(*v1alpha1.MachineClass)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotMachineClass)
	}

	fmt.Printf("Creating: %+v", cr)

	err := c.client.Omni().State().Create(ctx, machineClassForCR(cr))
	return managed.ExternalCreation{
		// Optionally return any details that may be required to connect to the
		// external resource. These will be stored as the connection secret.
		ConnectionDetails: managed.ConnectionDetails{},
	}, err
}

func (c *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	cr, ok := mg.(*v1alpha1.MachineClass)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errNotMachineClass)
	}

	fmt.Printf("Updating: %+v", cr)

	st := c.client.Omni().State()
	res, err := st.Get(ctx, pointerForCR(cr))
	if err != nil {
		return managed.ExternalUpdate{}, errors.Wrap(err, "failed to fetch resource")
	}

	oldMC, ok := res.(*resourcesomni.MachineClass)
	if !ok {
		return managed.ExternalUpdate{}, errors.New("resource is not a machineclass")
	}

	newMC := machineClassForCR(cr)
	err = syncCOSIMetadata(newMC, oldMC)
	if err != nil {
		return managed.ExternalUpdate{}, errors.Wrap(err, "failed to sync metadata")
	}

	err = st.Update(ctx, newMC)
	if err != nil {
		return managed.ExternalUpdate{}, errors.Wrap(err, "failed to update machineclass")
	}

	return managed.ExternalUpdate{
		// Optionally return any details that may be required to connect to the
		// external resource. These will be stored as the connection secret.
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

func (c *external) Delete(ctx context.Context, mg resource.Managed) (managed.ExternalDelete, error) {
	cr, ok := mg.(*v1alpha1.MachineClass)
	if !ok {
		return managed.ExternalDelete{}, errors.New(errNotMachineClass)
	}

	fmt.Printf("Deleting: %+v", cr)

	err := c.client.Omni().State().Destroy(ctx, pointerForCR(cr))
	return managed.ExternalDelete{}, err
}

func machineClassForCR(cr *v1alpha1.MachineClass) *resourcesomni.MachineClass {
	mc := resourcesomni.NewMachineClass(resources.DefaultNamespace, cr.Spec.ForProvider.Name)
	spec := mc.TypedSpec().Value
	fp := &cr.Spec.ForProvider
	spec.MatchLabels = make([]string, 0)
	spec.AutoProvision = &specs.MachineClassSpec_Provision{
		ProviderId: fp.AutoProvision.ProviderID,
		KernelArgs: fp.AutoProvision.KernelArgs,
		MetaValues: make([]*specs.MetaValue, 0),
		ProviderData: fmt.Sprintf("cores: %d\nmemory: %d\ndisk_size: %d\narchitecture: %s",
			fp.AutoProvision.Resources.CPU,
			fp.AutoProvision.Resources.Memory,
			fp.AutoProvision.Resources.DiskSize,
			fp.AutoProvision.Resources.Architecture,
		),
		GrpcTunnel: specs.GrpcTunnelMode_UNSET,
	}
	return mc
}

func pointerForCR(cr *v1alpha1.MachineClass) cosiresource.Pointer {
	return cosiresource.NewMetadata(resources.DefaultNamespace, resourcesomni.MachineClassType, cr.Spec.ForProvider.Name, cosiresource.VersionUndefined)
}

func syncCOSIMetadata(dst, src cosiresource.Resource) error {
	dst.Metadata().SetVersion(src.Metadata().Version())
	dst.Metadata().SetUpdated(src.Metadata().Updated())
	dst.Metadata().SetCreated(src.Metadata().Created())
	dst.Metadata().Finalizers().Set(*src.Metadata().Finalizers())
	err := dst.Metadata().SetOwner(src.Metadata().Owner())
	dst.Metadata().SetPhase(src.Metadata().Phase())
	return err
}
