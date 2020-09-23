package webhook

import (
	"flag"
	"fmt"

	"github.com/open-policy-agent/gatekeeper/pkg/util"
	authenticationv1 "k8s.io/api/authentication/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var log = logf.Log.WithName("webhook")

const (
	serviceAccountName = "gatekeeper-admin"
)

var (
	runtimeScheme                      = k8sruntime.NewScheme()
	codecs                             = serializer.NewCodecFactory(runtimeScheme)
	deserializer                       = codecs.UniversalDeserializer()
	disableEnforcementActionValidation = flag.Bool("disable-enforcementaction-validation", false, "disable validation of the enforcementAction field of a constraint")
	logDenies                          = flag.Bool("log-denies", false, "log detailed info on each deny")
	emitAdmissionEvents                = flag.Bool("emit-admission-events", false, "(alpha) emit Kubernetes events in gatekeeper namespace for each admission violation")
	serviceaccount                     = fmt.Sprintf("system:serviceaccount:%s:%s", util.GetNamespace(), serviceAccountName)
	// webhookName is deprecated, set this on the manifest YAML if needed"
)

func isGkServiceAccount(user authenticationv1.UserInfo) bool {
	return user.Username == serviceaccount
}
