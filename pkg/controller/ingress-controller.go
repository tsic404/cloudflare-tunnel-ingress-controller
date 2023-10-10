package controller

import (
	"context"
	"fmt"

	cloudflarecontroller "github.com/STRRL/cloudflare-tunnel-ingress-controller/pkg/cloudflare-controller"
	"github.com/STRRL/cloudflare-tunnel-ingress-controller/pkg/exposure"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// IngressController should implement the Reconciler interface
var _ reconcile.Reconciler = &IngressController{}

const WellKnownIngressAnnotation = "kubernetes.io/ingress.class"
const IngressControllerFinalizer = "strrl.dev/cloudflare-tunnel-ingress-controller-controlled"

type IngressController struct {
	logger              logr.Logger
	kubeClient          client.Client
	ingressClassName    string
	controllerClassName string
	tunnelClient        *cloudflarecontroller.TunnelClient
}

func NewIngressController(logger logr.Logger, kubeClient client.Client, ingressClassName string, controllerClassName string, tunnelClient *cloudflarecontroller.TunnelClient) *IngressController {
	return &IngressController{logger: logger, kubeClient: kubeClient, ingressClassName: ingressClassName, controllerClassName: controllerClassName, tunnelClient: tunnelClient}
}

func (i *IngressController) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	origin := networkingv1.Ingress{}
	err := i.kubeClient.Get(ctx, request.NamespacedName, &origin)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, errors.Wrapf(err, "fetch ingress %s", request.NamespacedName)
	}

	controlled, err := i.isControlledByThisController(ctx, origin)
	if err != nil && !apierrors.IsNotFound(err) {
		return reconcile.Result{}, errors.Wrapf(err, "check if ingress %s is controlled by this controller", request.NamespacedName)
	}

	if !controlled {
		i.logger.V(1).Info("ingress is NOT controlled by this controller",
			"ingress", request.NamespacedName,
			"controlled-ingress-class", i.ingressClassName,
			"controlled-controller-class", i.controllerClassName,
		)
		return reconcile.Result{
			Requeue: false,
		}, nil
	}

	i.logger.V(1).Info("ingress is controlled by this controller",
		"ingress", request.NamespacedName,
		"controlled-ingress-class", i.ingressClassName,
		"controlled-controller-class", i.controllerClassName,
	)

	i.logger.Info("update cloudflare tunnel config", "triggered-by", request.NamespacedName)

	ingresses, err := i.listControlledIngresses(ctx)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "list controlled ingresses")
	}

	var allExposures []exposure.Exposure
	for _, ingress := range ingresses {
		// best effort to extract exposures from all ingresses
		exposures, err := FromIngressToExposure(ctx, i.logger, i.kubeClient, ingress)
		if err != nil {
			i.logger.Info("extract exposures from ingress, skipped", "triggered-by", request.NamespacedName, "ingress", fmt.Sprintf("%s/%s", ingress.Namespace, ingress.Name), "error", err)
		}
		allExposures = append(allExposures, exposures...)
	}
	i.logger.V(3).Info("all exposures", "exposures", allExposures)

	err = i.tunnelClient.PutExposures(ctx, allExposures)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "put exposures")
	}

	i.logger.V(3).Info("reconcile completed", "triggered-by", request.NamespacedName)
	return reconcile.Result{}, nil
}

func (i *IngressController) isControlledByThisController(ctx context.Context, target networkingv1.Ingress) (bool, error) {
	if i.ingressClassName == target.GetAnnotations()[WellKnownIngressAnnotation] {
		return true, nil
	}

	if target.Spec.IngressClassName != nil && i.ingressClassName == *target.Spec.IngressClassName {
		return true, nil
	}

	return false, nil
}

func (i *IngressController) listControlledIngresses(ctx context.Context) ([]networkingv1.Ingress, error) {
	var result []networkingv1.Ingress
	list := networkingv1.IngressList{}
	err := i.kubeClient.List(ctx, &list)
	if err != nil {
		return nil, errors.Wrap(err, "list ingresses")
	}

	for _, ingress := range list.Items {
		func() {
			if i.ingressClassName == ingress.GetAnnotations()[WellKnownIngressAnnotation] {
				result = append(result, ingress)
				return
			}

			if ingress.Spec.IngressClassName != nil && i.ingressClassName == *ingress.Spec.IngressClassName {
				result = append(result, ingress)
				return
			}
		}()
	}

	return result, nil
}
