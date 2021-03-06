package ls

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/alb/rs"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/alb/tg"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/annotations"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/annotations/action"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/annotations/loadbalancer"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/controller/store"
	util "github.com/kubernetes-sigs/aws-alb-ingress-controller/pkg/util/types"
	extensions "k8s.io/api/extensions/v1beta1"
)

type ReconcileOptions struct {
	LBArn        string
	Ingress      *extensions.Ingress
	IngressAnnos *annotations.Ingress
	Port         loadbalancer.PortData
	TGGroup      tg.TargetGroupGroup

	// If instance is specified, reconcile will operate on this instance, otherwise new listener instance will be created.
	Instance *elbv2.Listener
}

type Controller interface {
	// Reconcile will make sure an AWS listener exists to satisfy requirements specified as options.
	Reconcile(ctx context.Context, options ReconcileOptions) error
}

func NewController(cloud aws.CloudAPI, store store.Storer, rulesController rs.Controller) Controller {
	return &defaultController{
		cloud:           cloud,
		store:           store,
		rulesController: rulesController,
	}
}

type defaultController struct {
	cloud aws.CloudAPI
	store store.Storer

	rulesController rs.Controller
}

type listenerConfig struct {
	Port           *int64
	Protocol       *string
	SslPolicy      *string
	Certificates   []*elbv2.Certificate
	DefaultActions []*elbv2.Action
}

func (controller *defaultController) Reconcile(ctx context.Context, options ReconcileOptions) error {
	config, err := controller.buildListenerConfig(ctx, options)
	if err != nil {
		return fmt.Errorf("failed to build listener config due to %v", err)
	}

	instance := options.Instance
	if instance == nil {
		if instance, err = controller.newLSInstance(ctx, options.LBArn, config); err != nil {
			return fmt.Errorf("failed to create listener due to %v", err)
		}
	} else {
		if instance, err = controller.reconcileLSInstance(ctx, instance, config); err != nil {
			return fmt.Errorf("failed to reconcile listener due to %v", err)
		}
	}
	if err := controller.rulesController.Reconcile(ctx, instance, options.Ingress, options.IngressAnnos, options.TGGroup); err != nil {
		return fmt.Errorf("failed to reconcile rules due to %v", err)
	}
	return nil
}

func (controller *defaultController) newLSInstance(ctx context.Context, lbArn string, config listenerConfig) (*elbv2.Listener, error) {
	resp, err := controller.cloud.CreateListenerWithContext(ctx, &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbArn),
		Port:            config.Port,
		Protocol:        config.Protocol,
		Certificates:    config.Certificates,
		SslPolicy:       config.SslPolicy,
		DefaultActions:  config.DefaultActions,
	})
	if err != nil {
		return nil, err
	}
	return resp.Listeners[0], nil
}

func (controller *defaultController) reconcileLSInstance(ctx context.Context, instance *elbv2.Listener, config listenerConfig) (*elbv2.Listener, error) {
	if controller.LSInstanceNeedsModification(ctx, instance, config) {
		output, err := controller.cloud.ModifyListenerWithContext(ctx, &elbv2.ModifyListenerInput{
			ListenerArn:    instance.ListenerArn,
			Port:           config.Port,
			Protocol:       config.Protocol,
			Certificates:   config.Certificates,
			SslPolicy:      config.SslPolicy,
			DefaultActions: config.DefaultActions,
		})
		if err != nil {
			return instance, err
		}
		return output.Listeners[0], nil
	}
	return instance, nil
}

func (controller *defaultController) LSInstanceNeedsModification(ctx context.Context, instance *elbv2.Listener, config listenerConfig) bool {
	needModification := false
	if !util.DeepEqual(instance.Port, config.Port) {
		needModification = true
	}
	if !util.DeepEqual(instance.Protocol, config.Protocol) {
		needModification = true
	}
	// TODO, check if we can compare this way!
	if !util.DeepEqual(instance.Certificates, config.Certificates) {
		needModification = true
	}
	if !util.DeepEqual(instance.SslPolicy, config.SslPolicy) {
		needModification = true
	}
	// TODO, check if we can compare this way!
	if !util.DeepEqual(instance.DefaultActions, config.DefaultActions) {
		needModification = true
	}
	return needModification
}

func (controller *defaultController) buildListenerConfig(ctx context.Context, options ReconcileOptions) (listenerConfig, error) {
	config := listenerConfig{
		Port:     aws.Int64(options.Port.Port),
		Protocol: aws.String(options.Port.Scheme),
	}
	if options.Port.Scheme == elbv2.ProtocolEnumHttps {
		if options.IngressAnnos.Listener.CertificateArn != nil {
			config.Certificates = []*elbv2.Certificate{
				{
					CertificateArn: options.IngressAnnos.Listener.CertificateArn,
					IsDefault:      aws.Bool(true),
				},
			}
		}
		if options.IngressAnnos.Listener.SslPolicy != nil {
			config.SslPolicy = options.IngressAnnos.Listener.SslPolicy
		}
	}
	actions, err := controller.buildDefaultActions(ctx, options)
	if err != nil {
		return config, err
	}
	config.DefaultActions = actions

	return config, nil
}

func (controller *defaultController) buildDefaultActions(ctx context.Context, options ReconcileOptions) ([]*elbv2.Action, error) {
	defaultBackend := options.Ingress.Spec.Backend
	if defaultBackend == nil {
		defaultBackend = action.Default404Backend()
	}
	if action.Use(defaultBackend.ServicePort.String()) {
		action, err := options.IngressAnnos.Action.GetAction(defaultBackend.ServiceName)
		if err != nil {
			return nil, err
		}
		return []*elbv2.Action{action}, nil
	}
	targetGroup, ok := options.TGGroup.TGByBackend[*defaultBackend]
	if !ok {
		return nil, fmt.Errorf("unable to find targetGroup for backend %v:%v",
			defaultBackend.ServiceName, defaultBackend.ServicePort.String())
	}
	action := &elbv2.Action{
		Type:           aws.String(elbv2.ActionTypeEnumForward),
		TargetGroupArn: aws.String(targetGroup.Arn),
	}
	return []*elbv2.Action{action}, nil
}
