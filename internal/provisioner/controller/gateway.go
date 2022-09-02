// Copyright Project Contour Authors
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

package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	contour_api_v1alpha1 "github.com/projectcontour/contour/apis/projectcontour/v1alpha1"
	"github.com/projectcontour/contour/internal/gatewayapi"
	"github.com/projectcontour/contour/internal/provisioner/model"
	"github.com/projectcontour/contour/internal/provisioner/objects/contourconfig"
	"github.com/projectcontour/contour/internal/provisioner/objects/dataplane"
	"github.com/projectcontour/contour/internal/provisioner/objects/deployment"
	"github.com/projectcontour/contour/internal/provisioner/objects/rbac"
	"github.com/projectcontour/contour/internal/provisioner/objects/secret"
	"github.com/projectcontour/contour/internal/provisioner/objects/service"
	retryable "github.com/projectcontour/contour/internal/provisioner/retryableerror"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	gatewayapi_v1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// gatewayReconciler reconciles Gateway objects.
type gatewayReconciler struct {
	gatewayController gatewayapi_v1beta1.GatewayController
	contourImage      string
	envoyImage        string
	client            client.Client
	log               logr.Logger
}

func NewGatewayController(mgr manager.Manager, gatewayController, contourImage, envoyImage string) (controller.Controller, error) {
	r := &gatewayReconciler{
		gatewayController: gatewayapi_v1beta1.GatewayController(gatewayController),
		contourImage:      contourImage,
		envoyImage:        envoyImage,
		client:            mgr.GetClient(),
		log:               ctrl.Log.WithName("gateway-controller"),
	}

	c, err := controller.New("gateway-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return nil, err
	}

	if err := c.Watch(
		&source.Kind{Type: &gatewayapi_v1beta1.Gateway{}},
		&handler.EnqueueRequestForObject{},
		predicate.NewPredicateFuncs(r.forReconcilableGatewayClass),
	); err != nil {
		return nil, err
	}

	// Watch GatewayClasses so we can trigger reconciles for any related
	// Gateways when a provisioner-controlled GatewayClass becomes
	// "Accepted: true".
	if err := c.Watch(
		&source.Kind{Type: &gatewayapi_v1beta1.GatewayClass{}},
		handler.EnqueueRequestsFromMapFunc(r.getGatewayClassGateways),
		predicate.NewPredicateFuncs(r.isGatewayClassReconcilable),
	); err != nil {
		return nil, err
	}

	return c, nil
}

// forReconcilableGatewayClass returns true if the provided Gateway uses a GatewayClass
// controlled by the provisioner, and that GatewayClass has a condition of
// "Accepted: true".
func (r *gatewayReconciler) forReconcilableGatewayClass(obj client.Object) bool {
	gw, ok := obj.(*gatewayapi_v1beta1.Gateway)
	if !ok {
		return false
	}

	gatewayClass := &gatewayapi_v1beta1.GatewayClass{}
	if err := r.client.Get(context.Background(), client.ObjectKey{Name: string(gw.Spec.GatewayClassName)}, gatewayClass); err != nil {
		return false
	}

	return r.isGatewayClassReconcilable(gatewayClass)
}

// isGatewayClassReconcilable returns true if the provided object is a
// GatewayClass controlled by the provisioner that has an "Accepted: true"
// condition.
func (r *gatewayReconciler) isGatewayClassReconcilable(obj client.Object) bool {
	gatewayClass, ok := obj.(*gatewayapi_v1beta1.GatewayClass)
	if !ok {
		return false
	}

	if gatewayClass.Spec.ControllerName != r.gatewayController {
		return false
	}

	var accepted bool
	for _, cond := range gatewayClass.Status.Conditions {
		if cond.Type == string(gatewayapi_v1beta1.GatewayClassConditionStatusAccepted) {
			if cond.Status == metav1.ConditionTrue {
				accepted = true
			}
			break
		}
	}

	return accepted
}

func (r *gatewayReconciler) getGatewayClassGateways(gatewayClass client.Object) []reconcile.Request {
	var gateways gatewayapi_v1beta1.GatewayList
	if err := r.client.List(context.Background(), &gateways); err != nil {
		r.log.Error(err, "error listing gateways")
		return nil
	}

	var reconciles []reconcile.Request
	for _, gw := range gateways.Items {
		if gw.Spec.GatewayClassName == gatewayapi_v1beta1.ObjectName(gatewayClass.GetName()) {
			reconciles = append(reconciles, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: gw.Namespace,
					Name:      gw.Name,
				},
			})
		}
	}

	return reconciles
}

func (r *gatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.log.WithValues("gateway-namespace", req.Namespace, "gateway-name", req.Name)

	gateway := &gatewayapi_v1beta1.Gateway{}
	if err := r.client.Get(ctx, req.NamespacedName, gateway); err != nil {
		if errors.IsNotFound(err) {
			log.Info("deleting gateway resources")

			contour := &model.Contour{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: req.Namespace,
					Name:      req.Name,
				},
			}

			if errs := r.ensureContourDeleted(ctx, contour, log); len(errs) > 0 {
				log.Error(utilerrors.NewAggregate(errs), "failed to delete resources for gateway")
			}

			return ctrl.Result{}, nil
		}
		// Error reading the object, so requeue the request.
		return ctrl.Result{}, fmt.Errorf("failed to get gateway %s: %w", req, err)
	}

	// Theoretically all event sources should be filtered already, but doesn't hurt
	// to double-check this here to ensure we only reconcile gateways for accepted
	// gateway classes the provisioner controls.
	gatewayClass := &gatewayapi_v1beta1.GatewayClass{}
	if err := r.client.Get(ctx, client.ObjectKey{Name: string(gateway.Spec.GatewayClassName)}, gatewayClass); err != nil {
		return ctrl.Result{}, fmt.Errorf("error getting gateway's gateway class: %w", err)
	}
	if !r.isGatewayClassReconcilable(gatewayClass) {
		return ctrl.Result{}, nil
	}

	log.Info("ensuring gateway resources")

	contourModel := model.Default(gateway.Namespace, gateway.Name)

	// Currently, only a single address of type IPAddress or Hostname
	// is supported; anything else will be ignored.
	if len(gateway.Spec.Addresses) > 0 {
		address := gateway.Spec.Addresses[0]

		if address.Type == nil ||
			*address.Type == gatewayapi_v1beta1.IPAddressType ||
			*address.Type == gatewayapi_v1beta1.HostnameAddressType {
			contourModel.Spec.NetworkPublishing.Envoy.LoadBalancer.LoadBalancerIP = address.Value
		}
	}

	// Validate listener ports and hostnames to get
	// the ports to program.
	validateListenersResult := gatewayapi.ValidateListeners(gateway.Spec.Listeners)

	if validateListenersResult.InsecurePort > 0 {
		port := model.ServicePort{
			Name:       "http",
			PortNumber: int32(validateListenersResult.InsecurePort),
		}
		contourModel.Spec.NetworkPublishing.Envoy.ServicePorts = append(contourModel.Spec.NetworkPublishing.Envoy.ServicePorts, port)
	}
	if validateListenersResult.SecurePort > 0 {
		port := model.ServicePort{
			Name:       "https",
			PortNumber: int32(validateListenersResult.SecurePort),
		}

		contourModel.Spec.NetworkPublishing.Envoy.ServicePorts = append(contourModel.Spec.NetworkPublishing.Envoy.ServicePorts, port)
	}

	gatewayClassParams, err := r.getGatewayClassParams(ctx, gatewayClass)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error getting gateway's gateway class parameters: %w", err)
	}

	if gatewayClassParams != nil {
		// ContourConfiguration
		contourModel.Spec.RuntimeSettings = gatewayClassParams.Spec.RuntimeSettings

		if gatewayClassParams.Spec.Contour != nil {
			// Deployment replicas
			if gatewayClassParams.Spec.Contour.Replicas > 0 {
				contourModel.Spec.ContourReplicas = gatewayClassParams.Spec.Contour.Replicas
			}

			// Node placement
			if nodePlacement := gatewayClassParams.Spec.Contour.NodePlacement; nodePlacement != nil {
				if contourModel.Spec.NodePlacement == nil {
					contourModel.Spec.NodePlacement = &model.NodePlacement{}
				}

				contourModel.Spec.NodePlacement.Contour = &model.ContourNodePlacement{
					NodeSelector: nodePlacement.NodeSelector,
					Tolerations:  nodePlacement.Tolerations,
				}
			}

			contourModel.Spec.LogLevel = gatewayClassParams.Spec.Contour.LogLevel
		}

		if gatewayClassParams.Spec.Envoy != nil {
			// Workload type
			// Note, the values have already been validated by the gatewayclass controller
			// so just check for the existence of a value here.
			if gatewayClassParams.Spec.Envoy.WorkloadType != "" {
				contourModel.Spec.EnvoyWorkloadType = gatewayClassParams.Spec.Envoy.WorkloadType
			}

			// Deployment replicas
			if gatewayClassParams.Spec.Envoy.WorkloadType == contour_api_v1alpha1.WorkloadTypeDeployment &&
				gatewayClassParams.Spec.Envoy.Replicas > 0 {
				contourModel.Spec.EnvoyReplicas = gatewayClassParams.Spec.Envoy.Replicas
			}

			// Network publishing
			if networkPublishing := gatewayClassParams.Spec.Envoy.NetworkPublishing; networkPublishing != nil {
				// Note, the values have already been validated by the gatewayclass controller
				// so just check for the existence of a value here.
				if networkPublishing.Type != "" {
					contourModel.Spec.NetworkPublishing.Envoy.Type = networkPublishing.Type
				}
				contourModel.Spec.NetworkPublishing.Envoy.ServiceAnnotations = networkPublishing.ServiceAnnotations
			}

			// Node placement
			if nodePlacement := gatewayClassParams.Spec.Envoy.NodePlacement; nodePlacement != nil {
				if contourModel.Spec.NodePlacement == nil {
					contourModel.Spec.NodePlacement = &model.NodePlacement{}
				}

				contourModel.Spec.NodePlacement.Envoy = &model.EnvoyNodePlacement{
					NodeSelector: nodePlacement.NodeSelector,
					Tolerations:  nodePlacement.Tolerations,
				}
			}
		}
	}

	if errs := r.ensureContour(ctx, contourModel, log); len(errs) > 0 {
		return ctrl.Result{}, fmt.Errorf("failed to ensure resources for gateway: %w", retryable.NewMaybeRetryableAggregate(errs))
	}

	var newConds []metav1.Condition
	for _, cond := range gateway.Status.Conditions {
		if cond.Type == string(gatewayapi_v1beta1.GatewayConditionScheduled) {
			if cond.Status == metav1.ConditionTrue {
				return ctrl.Result{}, nil
			}

			continue
		}

		newConds = append(newConds, cond)
	}

	log.Info("setting gateway's Scheduled condition to true")

	// nolint:gocritic
	gateway.Status.Conditions = append(newConds, metav1.Condition{
		Type:               string(gatewayapi_v1beta1.GatewayConditionScheduled),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gateway.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             string(gatewayapi_v1beta1.GatewayReasonScheduled),
		Message:            "Gateway is scheduled",
	})

	if err := r.client.Status().Update(ctx, gateway); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set gateway %s scheduled condition: %w", req, err)
	}

	return ctrl.Result{}, nil
}

func (r *gatewayReconciler) ensureContour(ctx context.Context, contour *model.Contour, log logr.Logger) []error {
	var errs []error

	handleResult := func(resource string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to ensure %s for gateway %s/%s: %w", resource, contour.Namespace, contour.Name, err))
		} else {
			log.Info(fmt.Sprintf("ensured %s for gateway", resource))
		}
	}

	handleResult("rbac", rbac.EnsureRBAC(ctx, r.client, contour))

	if len(errs) > 0 {
		return errs
	}

	handleResult("contour config", contourconfig.EnsureContourConfig(ctx, r.client, contour))
	handleResult("xDS TLS secrets", secret.EnsureXDSSecrets(ctx, r.client, contour, r.contourImage))
	handleResult("deployment", deployment.EnsureDeployment(ctx, r.client, contour, r.contourImage))
	handleResult("envoy data plane", dataplane.EnsureDataPlane(ctx, r.client, contour, r.contourImage, r.envoyImage))
	handleResult("contour service", service.EnsureContourService(ctx, r.client, contour))

	switch contour.Spec.NetworkPublishing.Envoy.Type {
	case model.LoadBalancerServicePublishingType, model.NodePortServicePublishingType, model.ClusterIPServicePublishingType:
		handleResult("envoy service", service.EnsureEnvoyService(ctx, r.client, contour))
	}

	return errs
}

func (r *gatewayReconciler) ensureContourDeleted(ctx context.Context, contour *model.Contour, log logr.Logger) []error {
	var errs []error

	handleResult := func(resource string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to delete %s for gateway %s/%s: %w", resource, contour.Namespace, contour.Name, err))
		} else {
			log.Info(fmt.Sprintf("deleted %s for gateway", resource))
		}
	}

	handleResult("envoy service", service.EnsureEnvoyServiceDeleted(ctx, r.client, contour))
	handleResult("service", service.EnsureContourServiceDeleted(ctx, r.client, contour))
	handleResult("envoy data plane", dataplane.EnsureDataPlaneDeleted(ctx, r.client, contour))
	handleResult("deployment", deployment.EnsureDeploymentDeleted(ctx, r.client, contour))
	handleResult("xDS TLS Secrets", secret.EnsureXDSSecretsDeleted(ctx, r.client, contour))
	handleResult("contour config", contourconfig.EnsureContourConfigDeleted(ctx, r.client, contour))
	handleResult("rbac", rbac.EnsureRBACDeleted(ctx, r.client, contour))

	return errs
}

func (r *gatewayReconciler) getGatewayClassParams(ctx context.Context, gatewayClass *gatewayapi_v1beta1.GatewayClass) (*contour_api_v1alpha1.ContourDeployment, error) {
	// Check if there is a parametersRef to ContourDeployment with
	// a namespace specified. Theoretically, we should only be reconciling
	// Gateways for GatewayClasses that have valid parameter refs (or no refs),
	// making this check mostly redundant other than checking for a nil params
	// ref, but there is potentially a race condition where a GatewayClass's
	// parameters ref is updated from valid to invalid, and then a Gateway reconcile
	// is triggered before the GatewayClass's status is updated, that
	// would lead to this code being executed for a GatewayClass with an
	// invalid parametersRef.
	if !isContourDeploymentRef(gatewayClass.Spec.ParametersRef) {
		return nil, nil
	}

	gcParams := &contour_api_v1alpha1.ContourDeployment{}
	key := client.ObjectKey{
		Namespace: string(*gatewayClass.Spec.ParametersRef.Namespace),
		Name:      gatewayClass.Spec.ParametersRef.Name,
	}

	if err := r.client.Get(ctx, key, gcParams); err != nil {
		return nil, fmt.Errorf("error getting gateway class's parameters: %w", err)
	}

	return gcParams, nil

}
