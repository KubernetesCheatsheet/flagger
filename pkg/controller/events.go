package controller

import (
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1 "k8s.io/api/core/v1"

	flaggerv1 "github.com/weaveworks/flagger/pkg/apis/flagger/v1beta1"
	"github.com/weaveworks/flagger/pkg/notifier"
)

func (c *Controller) recordEventInfof(r *flaggerv1.Canary, template string, args ...interface{}) {
	c.logger.With("canary", fmt.Sprintf("%s.%s", r.Name, r.Namespace)).Infof(template, args...)
	c.eventRecorder.Event(r, corev1.EventTypeNormal, "Synced", fmt.Sprintf(template, args...))
	c.sendEventToWebhook(r, corev1.EventTypeNormal, template, args)
}

func (c *Controller) recordEventErrorf(r *flaggerv1.Canary, template string, args ...interface{}) {
	c.logger.With("canary", fmt.Sprintf("%s.%s", r.Name, r.Namespace)).Errorf(template, args...)
	c.eventRecorder.Event(r, corev1.EventTypeWarning, "Synced", fmt.Sprintf(template, args...))
	c.sendEventToWebhook(r, corev1.EventTypeWarning, template, args)
}

func (c *Controller) recordEventWarningf(r *flaggerv1.Canary, template string, args ...interface{}) {
	c.logger.With("canary", fmt.Sprintf("%s.%s", r.Name, r.Namespace)).Infof(template, args...)
	c.eventRecorder.Event(r, corev1.EventTypeWarning, "Synced", fmt.Sprintf(template, args...))
	c.sendEventToWebhook(r, corev1.EventTypeWarning, template, args)
}

func (c *Controller) sendEventToWebhook(r *flaggerv1.Canary, eventType, template string, args []interface{}) {
	webhookOverride := false
	if len(r.Spec.CanaryAnalysis.Webhooks) > 0 {
		for _, canaryWebhook := range r.Spec.CanaryAnalysis.Webhooks {
			if canaryWebhook.Type == flaggerv1.EventHook {
				webhookOverride = true
				err := CallEventWebhook(r, canaryWebhook.URL, fmt.Sprintf(template, args...), eventType)
				if err != nil {
					c.logger.With("canary", fmt.Sprintf("%s.%s", r.Name, r.Namespace)).Errorf("error sending event to webhook: %s", err)
				}
			}
		}
	}

	if c.eventWebhook != "" && !webhookOverride {
		err := CallEventWebhook(r, c.eventWebhook, fmt.Sprintf(template, args...), eventType)
		if err != nil {
			c.logger.With("canary", fmt.Sprintf("%s.%s", r.Name, r.Namespace)).Errorf("error sending event to webhook: %s", err)
		}
	}
}

func (c *Controller) alert(canary *flaggerv1.Canary, message string, metadata bool, severity flaggerv1.AlertSeverity) {
	if c.notifier == nil && len(canary.Spec.CanaryAnalysis.Alerts) == 0 {
		return
	}

	var fields []notifier.Field
	if metadata {
		fields = alertMetadata(canary)
	}

	// send alert with the global notifier
	if len(canary.Spec.CanaryAnalysis.Alerts) == 0 {
		err := c.notifier.Post(canary.Name, canary.Namespace, message, fields, string(severity))
		if err != nil {
			c.logger.With("canary", fmt.Sprintf("%s.%s", canary.Name, canary.Namespace)).
				Errorf("alert can't be sent: %v", err)
			return
		}
		return
	}

	// send canary alerts
	for _, alert := range canary.Spec.CanaryAnalysis.Alerts {
		// determine if alert should be sent based on severity level
		shouldAlert := false
		if alert.Severity == flaggerv1.SeverityInfo {
			shouldAlert = true
		} else {
			if severity == alert.Severity {
				shouldAlert = true
			}
			if severity == flaggerv1.SeverityWarn && alert.Severity == flaggerv1.SeverityError {
				shouldAlert = true
			}
		}
		if !shouldAlert {
			continue
		}

		// determine alert provider namespace
		providerNamespace := canary.GetNamespace()
		if alert.ProviderRef.Namespace != "" {
			providerNamespace = alert.ProviderRef.Namespace
		}

		// find alert provider
		provider, err := c.flaggerInformers.AlertInformer.Lister().AlertProviders(providerNamespace).Get(alert.ProviderRef.Name)
		if err != nil {
			c.logger.With("canary", fmt.Sprintf("%s.%s", canary.Name, canary.Namespace)).
				Errorf("alert provider %s.%s error: %v", alert.ProviderRef.Name, providerNamespace, err)
			continue
		}

		// set hook URL address
		url := provider.Spec.Address

		// extract address from secret
		if provider.Spec.SecretRef != nil {
			secret, err := c.kubeClient.CoreV1().Secrets(providerNamespace).Get(provider.Spec.SecretRef.Name, metav1.GetOptions{})
			if err != nil {
				c.logger.With("canary", fmt.Sprintf("%s.%s", canary.Name, canary.Namespace)).
					Errorf("alert provider %s.%s secretRef error: %v", alert.ProviderRef.Name, providerNamespace, err)
				continue
			}
			if address, ok := secret.Data["address"]; ok {
				url = string(address)
			} else {
				c.logger.With("canary", fmt.Sprintf("%s.%s", canary.Name, canary.Namespace)).
					Errorf("alert provider %s.%s secret does not contain an address", alert.ProviderRef.Name, providerNamespace)
				continue
			}
		}

		// set defaults
		username := "flagger"
		if provider.Spec.Username != "" {
			username = provider.Spec.Username
		}
		channel := "general"
		if provider.Spec.Channel != "" {
			channel = provider.Spec.Channel
		}

		// create notifier based on provider type
		f := notifier.NewFactory(url, username, channel)
		n, err := f.Notifier(provider.Spec.Type)
		if err != nil {
			c.logger.With("canary", fmt.Sprintf("%s.%s", canary.Name, canary.Namespace)).
				Errorf("alert provider %s.%s error: %v", alert.ProviderRef.Name, providerNamespace, err)
			continue
		}

		// send alert
		err = n.Post(canary.Name, canary.Namespace, message, fields, string(severity))
		if err != nil {
			c.logger.With("canary", fmt.Sprintf("%s.%s", canary.Name, canary.Namespace)).
				Errorf("alert provider $s.%s send error: %v", alert.ProviderRef.Name, providerNamespace, err)
		}

	}
}

func alertMetadata(canary *flaggerv1.Canary) []notifier.Field {
	var fields []notifier.Field
	fields = append(fields,
		notifier.Field{
			Name:  "Target",
			Value: fmt.Sprintf("%s/%s.%s", canary.Spec.TargetRef.Kind, canary.Spec.TargetRef.Name, canary.Namespace),
		},
		notifier.Field{
			Name:  "Failed checks threshold",
			Value: fmt.Sprintf("%v", canary.Spec.CanaryAnalysis.Threshold),
		},
		notifier.Field{
			Name:  "Progress deadline",
			Value: fmt.Sprintf("%vs", canary.GetProgressDeadlineSeconds()),
		},
	)

	if canary.Spec.CanaryAnalysis.StepWeight > 0 {
		fields = append(fields, notifier.Field{
			Name: "Traffic routing",
			Value: fmt.Sprintf("Weight step: %v max: %v",
				canary.Spec.CanaryAnalysis.StepWeight,
				canary.Spec.CanaryAnalysis.MaxWeight),
		})
	} else if len(canary.Spec.CanaryAnalysis.Match) > 0 {
		fields = append(fields, notifier.Field{
			Name:  "Traffic routing",
			Value: "A/B Testing",
		})
	} else if canary.Spec.CanaryAnalysis.Iterations > 0 {
		fields = append(fields, notifier.Field{
			Name:  "Traffic routing",
			Value: "Blue/Green",
		})
	}
	return fields
}
