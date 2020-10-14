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
	"flag"
	"net/http"
	"time"

	"gomodules.xyz/jsonpatch/v2"

	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	opa "github.com/open-policy-agent/frameworks/constraint/pkg/client"
	"github.com/open-policy-agent/gatekeeper/apis"
	"github.com/open-policy-agent/gatekeeper/pkg/controller/config/process"
	"github.com/open-policy-agent/gatekeeper/pkg/util"
	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	clientcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type mutationResponse string

// MutationEnabled indicates if the mutation feature is enabled
var MutationEnabled *bool

const (
	mutationErrorResponse   mutationResponse = "error"
	mutationDenyResponse    mutationResponse = "deny"
	mutationAllowResponse   mutationResponse = "allow"
	mutationSkipResponse    mutationResponse = "skip"
	mutationUnknownResponse mutationResponse = "unknown"
)

func init() {
	MutationEnabled = flag.Bool("enable-mutation", false, "Enable the mutation webhook")

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
	if !*MutationEnabled {
		return nil
	}
	reporter, err := newStatsReporter()
	if err != nil {
		return err
	}
	eventBroadcaster := record.NewBroadcaster()
	kubeClient := kubernetes.NewForConfigOrDie(mgr.GetConfig())

	eventBroadcaster.StartRecordingToSink(&clientcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(
		scheme.Scheme,
		corev1.EventSource{Component: "gatekeeper-mutation-webhook"})

	wh := &admission.Webhook{
		Handler: &mutationHandler{
			webhookHandler: webhookHandler{
				opa:             opa,
				client:          mgr.GetClient(),
				reader:          mgr.GetAPIReader(),
				reporter:        reporter,
				processExcluder: processExcluder,
				eventRecorder:   recorder,
				gkNamespace:     util.GetNamespace(),
			},
		},
	}

	// TODO(https://github.com/open-policy-agent/gatekeeper/issues/661): remove log injection if the race condition in the cited bug is eliminated.
	// Otherwise we risk having unstable logger names for the webhook.
	if err := wh.InjectLogger(log); err != nil {
		return err
	}
	mgr.GetWebhookServer().Register("/v1/mutate", wh)

	return nil
}

var _ admission.Handler = &mutationHandler{}

type mutationHandler struct {
	webhookHandler
}

// Handle the validation request
func (h *mutationHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
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

	resp, err := h.mutateRequest(ctx, req)
	if err != nil {
		log.Error(err, "error executing query")
		vResp := admission.Response{
			AdmissionResponse: admissionv1beta1.AdmissionResponse{
				Allowed: true,
				Result: &metav1.Status{
					Code: int32(http.StatusInternalServerError),
				},
			},
		}
		return vResp
	}
	return resp
}

// traceSwitch returns true if a request should be traced
func (h *mutationHandler) mutateRequest(ctx context.Context, req admission.Request) (admission.Response, error) {

	// TODO: place mutation logic here
	patches := []jsonpatch.JsonPatchOperation{}
	resp := admission.Response{
		AdmissionResponse: admissionv1beta1.AdmissionResponse{
			Allowed: true,
			Result: &metav1.Status{
				Code: int32(http.StatusOK),
			},
		},
		Patches: patches,
	}
	return resp, nil
}

func AppendMutationWebhookIfEnabled(webhooks []rotator.WebhookInfo) []rotator.WebhookInfo {
	if *MutationEnabled {
		return append(webhooks, rotator.WebhookInfo{
			Name: MwhName,
			Type: rotator.MutingWebhook,
		})
	}
	return webhooks
}
