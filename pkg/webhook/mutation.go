/*
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

package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	opa "github.com/open-policy-agent/frameworks/constraint/pkg/client"
	rtypes "github.com/open-policy-agent/frameworks/constraint/pkg/types"
	"github.com/open-policy-agent/gatekeeper/apis"
	"github.com/open-policy-agent/gatekeeper/apis/config/v1alpha1"
	"github.com/open-policy-agent/gatekeeper/pkg/controller/config/process"
	"github.com/open-policy-agent/gatekeeper/pkg/keys"
	"github.com/open-policy-agent/gatekeeper/pkg/target"
	"github.com/open-policy-agent/gatekeeper/pkg/util"
	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	clientcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type mutationResponse string

const (
	mutationErrorResponse   mutationResponse = "error"
	mutationDenyResponse    mutationResponse = "deny"
	mutationAllowResponse   mutationResponse = "allow"
	mutationSkipResponse    mutationResponse = "skip"
	mutationUnknownResponse mutationResponse = "unknown"
)

func init() {
	log.Info("!!!!!!!!!!!!!!!  AddToManagerFuncs AddMutatingWebhook")

	AddToManagerFuncs = append(AddToManagerFuncs, AddMutatingWebhook)

	if err := apis.AddToScheme(runtimeScheme); err != nil {
		log.Error(err, "unable to add to scheme")
		panic(err)
	}
}

// +kubebuilder:webhook:verbs=create,path=/v1/mutate,mutating=true,failurePolicy=ignore,groups=*,resources=*,versions=*,name=mutation.gatekeeper.sh
// +kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch;update

// AddMutatingWebhook registers the mutating webhook server with the manager
func AddMutatingWebhook(mgr manager.Manager, opa *opa.Client, processExcluder *process.Excluder) error {
	log.Info("!!!!!!!!!!!!!!! AddMutatingWebhook")

	reporter, err := newStatsReporter()
	if err != nil {
		return err
	}
	log.Info("!!!!!!!!!!!!!!! AddMutatingWebhook   1")

	eventBroadcaster := record.NewBroadcaster()
	log.Info("!!!!!!!!!!!!!!! AddMutatingWebhook   2")

	kubeClient := kubernetes.NewForConfigOrDie(mgr.GetConfig())
	log.Info("!!!!!!!!!!!!!!! AddMutatingWebhook   3")

	eventBroadcaster.StartRecordingToSink(&clientcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(
		scheme.Scheme,
		corev1.EventSource{Component: "gatekeeper-mutation-webhook"})
	log.Info("!!!!!!!!!!!!!!! AddMutatingWebhook   4")

	wh := &admission.Webhook{
		Handler: &validationHandler{
			opa:             opa,
			client:          mgr.GetClient(),
			reader:          mgr.GetAPIReader(),
			reporter:        reporter,
			processExcluder: processExcluder,
			eventRecorder:   recorder,
			gkNamespace:     util.GetNamespace(),
		},
	}
	log.Info("!!!!!!!!!!!!!!! AddMutatingWebhook   5")

	// TODO(https://github.com/open-policy-agent/gatekeeper/issues/661): remove log injection if the race condition in the cited bug is eliminated.
	// Otherwise we risk having unstable logger names for the webhook.
	if err := wh.InjectLogger(log); err != nil {
		return err
	}
	log.Info("!!!!!!!!!!!!!!! AddMutatingWebhook   6")

	mgr.GetWebhookServer().Register("/v1/mutate", wh)
	log.Info("!!!!!!!!!!!!!!! AddMutatingWebhook   7")

	return nil
}

var _ admission.Handler = &mutationHandler{}

type mutationHandler struct {
	opa      *opa.Client
	client   client.Client
	reporter StatsReporter
	// reader that will be configured to use the API server
	// obtained from mgr.GetAPIReader()
	reader client.Reader
	// for testing
	injectedConfig  *v1alpha1.Config
	processExcluder *process.Excluder
	eventRecorder   record.EventRecorder
	gkNamespace     string
}

// Handle the validation request
func (h *mutationHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	log.Info("!!!!!!!!!! MUTATION           ")

	log := log.WithValues("hookType", "mutation")

	var timeStart = time.Now()

	if isGkServiceAccount(req.AdmissionRequest.UserInfo) {
		return admission.ValidationResponse(true, "Gatekeeper does not self-manage")
	}

	if req.AdmissionRequest.Operation != admissionv1beta1.Create &&
		req.AdmissionRequest.Operation != admissionv1beta1.Update {
		return admission.ValidationResponse(true, "Mutating only on create")
	}

	if h.isGatekeeperResource(ctx, req) {
		return admission.ValidationResponse(true, "Not mutating gatekeeper resources")
	}

	requestResponse := mutationUnknownResponse
	defer func() {
		if h.reporter != nil {
			if err := h.reporter.ReportMutationRequest(
				requestResponse, time.Since(timeStart)); err != nil {
				log.Error(err, "failed to report request")
			}
		}
	}()

	// namespace is excluded from webhook using config
	if h.skipExcludedNamespace(req.AdmissionRequest.Namespace) {
		requestResponse = mutationSkipResponse
		return admission.ValidationResponse(true, "Namespace is set to be ignored by Gatekeeper config")
	}

	resp, err := h.reviewRequest(ctx, req)
	if err != nil {
		log.Error(err, "error executing query")
		vResp := admission.ValidationResponse(false, err.Error())
		if vResp.Result == nil {
			vResp.Result = &metav1.Status{}
		}
		vResp.Result.Code = http.StatusInternalServerError
		requestResponse = mutationErrorResponse
		return vResp
	}

	res := resp.Results()
	msgs := h.getDenyMessages(res, req)
	if len(msgs) > 0 {
		vResp := admission.ValidationResponse(false, strings.Join(msgs, "\n"))
		if vResp.Result == nil {
			vResp.Result = &metav1.Status{}
		}
		vResp.Result.Code = http.StatusForbidden
		requestResponse = mutationDenyResponse
		return vResp
	}

	requestResponse = mutationAllowResponse
	return admission.ValidationResponse(true, "")
}

func (h *mutationHandler) getDenyMessages(res []*rtypes.Result, req admission.Request) []string {
	var msgs []string
	var resourceName string
	if len(res) > 0 && (*logDenies || *emitAdmissionEvents) {
		resourceName = req.AdmissionRequest.Name
		if len(resourceName) == 0 && req.AdmissionRequest.Object.Raw != nil {
			// On a CREATE operation, the client may omit name and
			// rely on the server to generate the name.
			obj := &unstructured.Unstructured{}
			if _, _, err := deserializer.Decode(req.AdmissionRequest.Object.Raw, nil, obj); err == nil {
				resourceName = obj.GetName()
			}
		}
	}
	for _, r := range res {
		if r.EnforcementAction == "deny" || r.EnforcementAction == "dryrun" {
			if *logDenies {
				log.WithValues(
					"process", "admission",
					"event_type", "violation",
					"constraint_name", r.Constraint.GetName(),
					"constraint_kind", r.Constraint.GetKind(),
					"constraint_action", r.EnforcementAction,
					"resource_kind", req.AdmissionRequest.Kind.Kind,
					"resource_namespace", req.AdmissionRequest.Namespace,
					"resource_name", resourceName,
					"request_username", req.AdmissionRequest.UserInfo.Username,
				).Info("denied admission")
			}
			if *emitAdmissionEvents {
				annotations := map[string]string{
					"process":            "admission",
					"event_type":         "violation",
					"constraint_name":    r.Constraint.GetName(),
					"constraint_kind":    r.Constraint.GetKind(),
					"constraint_action":  r.EnforcementAction,
					"resource_kind":      req.AdmissionRequest.Kind.Kind,
					"resource_namespace": req.AdmissionRequest.Namespace,
					"resource_name":      resourceName,
					"request_username":   req.AdmissionRequest.UserInfo.Username,
				}
				eventMsg := "Admission webhook \"validation.gatekeeper.sh\" denied request"
				reason := "FailedAdmission"
				if r.EnforcementAction == "dryrun" {
					eventMsg = "Dryrun violation"
					reason = "DryrunViolation"
				}
				ref := getViolationRef(h.gkNamespace, req.AdmissionRequest.Kind.Kind, resourceName, req.AdmissionRequest.Namespace, r.Constraint.GetKind(), r.Constraint.GetName(), r.Constraint.GetNamespace())
				h.eventRecorder.AnnotatedEventf(ref, annotations, corev1.EventTypeWarning, reason, "%s, Resource Namespace: %s, Constraint: %s, Message: %s", eventMsg, req.AdmissionRequest.Namespace, r.Constraint.GetName(), r.Msg)
			}

		}
		// only deny enforcementAction should prompt deny admission response
		if r.EnforcementAction == "deny" {
			msgs = append(msgs, fmt.Sprintf("[denied by %s] %s", r.Constraint.GetName(), r.Msg))
		}
	}
	return msgs
}

func (h *mutationHandler) getConfig(ctx context.Context) (*v1alpha1.Config, error) {
	if h.injectedConfig != nil {
		return h.injectedConfig, nil
	}
	if h.client == nil {
		return nil, errors.New("no client available to retrieve validation config")
	}
	cfg := &v1alpha1.Config{}
	return cfg, h.client.Get(ctx, keys.Config, cfg)
}

// validateGatekeeperResources returns whether an issue is user error (vs internal) and any errors
// validating internal resources
func (h *mutationHandler) isGatekeeperResource(ctx context.Context, req admission.Request) bool {
	if req.AdmissionRequest.Kind.Group == "templates.gatekeeper.sh" ||
		req.AdmissionRequest.Kind.Group == "constraints.gatekeeper.sh" {
		return true
	}

	return false
}

// traceSwitch returns true if a request should be traced
func (h *mutationHandler) reviewRequest(ctx context.Context, req admission.Request) (*rtypes.Responses, error) {
	trace, dump := h.tracingLevel(ctx, req)
	// Coerce server-side apply admission requests into treating namespaces
	// the same way as older admission requests. See
	// https://github.com/open-policy-agent/gatekeeper/issues/792
	if req.Kind.Kind == "Namespace" && req.Kind.Group == "" {
		req.Namespace = ""
	}
	review := &target.AugmentedReview{AdmissionRequest: &req.AdmissionRequest}
	if req.AdmissionRequest.Namespace != "" {
		ns := &corev1.Namespace{}
		if err := h.client.Get(ctx, types.NamespacedName{Name: req.AdmissionRequest.Namespace}, ns); err != nil {
			if !k8serrors.IsNotFound(err) {
				return nil, err
			}
			// bypass cached client and ask api-server directly
			err = h.reader.Get(ctx, types.NamespacedName{Name: req.AdmissionRequest.Namespace}, ns)
			if err != nil {
				return nil, err
			}
		}
		review.Namespace = ns
	}

	resp, err := h.opa.Review(ctx, review, opa.Tracing(trace))
	// TODO MUTATE HERE
	if trace {
		log.Info(resp.TraceDump())
	}
	if dump {
		dump, err := h.opa.Dump(ctx)
		if err != nil {
			log.Error(err, "dump error")
		} else {
			log.Info(dump)
		}
	}
	return resp, err
}

func (h *mutationHandler) tracingLevel(ctx context.Context, req admission.Request) (bool, bool) {
	cfg, _ := h.getConfig(ctx)
	traceEnabled := false
	dump := false
	for _, trace := range cfg.Spec.Validation.Traces {
		if trace.User != req.AdmissionRequest.UserInfo.Username {
			continue
		}
		gvk := v1alpha1.GVK{
			Group:   req.AdmissionRequest.Kind.Group,
			Version: req.AdmissionRequest.Kind.Version,
			Kind:    req.AdmissionRequest.Kind.Kind,
		}
		if gvk == trace.Kind {
			traceEnabled = true
			if strings.EqualFold(trace.Dump, "All") {
				dump = true
			}
		}
	}
	return traceEnabled, dump
}

func (h *mutationHandler) skipExcludedNamespace(namespace string) bool {
	return h.processExcluder.IsNamespaceExcluded(process.Webhook, namespace)
}
