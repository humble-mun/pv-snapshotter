package webhook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/humble-mun/pv-snapshotter/pkg/annotation"
)

// ---------------------------------------------------------------------------
// Flag names and defaults
// ---------------------------------------------------------------------------

const (
	flagPVCNameTemplate     = "webhook-pvc-name-template"
	flagPVCSelectorTemplate = "webhook-pvc-selector-template"
	flagMaxOwnerDepth       = "webhook-max-owner-depth"
	flagDefaultRuntimeClass = "webhook-default-runtime-class"
	flagRuntimeClassSuffix  = "webhook-runtime-class-suffix"
	flagAnnotationTemplates = "webhook-annotation-templates"
	flagStateMountPath      = "webhook-state-mount-path"
	flagBoundTimeout        = "webhook-bound-timeout"
	flagEnabled             = "webhook-enabled"

	defaultPVCNameTemplate     = "{{.OwnerName}}"
	defaultPVCSelectorTemplate = ""
	defaultMaxOwnerDepth       = 2
	defaultDefaultRuntimeClass = ""
	defaultRuntimeClassSuffix  = "-pv"
	defaultStateMountPath      = "/.platform/state"
	defaultBoundTimeout        = 30 * time.Second
	defaultEnabled             = true

	// annotationSuffixPVCNameTemplate is the fixed annotation key suffix for the
	// per-pod PVC-name override. The full key is derived at construction time as
	// "<annotation-prefix>/pvc-name-template" (annotation.Key), so the prefix is
	// configurable via --annotation-prefix while the suffix stays constant.
	annotationSuffixPVCNameTemplate = "pvc-name-template"

	// statePVCVolumeName is the volume name the webhook injects for the
	// state PVC. The double-dash vendor-separator prefix makes it collision-
	// resistant: user-defined volume names almost never contain "--", so
	// this name is unlikely to clash with an existing volume in the pod.
	statePVCVolumeName = "pv-snapshotter--state"

	// podIndexLabel is the well-known label Kubernetes sets on StatefulSet
	// pods and Indexed Job pods (1.28+, KEP-2164) holding the pod's ordinal /
	// completion index. It is read as a plain pod label rather than parsed
	// out of the pod name so the value keeps working regardless of naming
	// scheme.
	podIndexLabel = "apps.kubernetes.io/pod-index"

	// boundPollInterval is the pause between PVC phase re-checks.
	boundPollInterval = 2 * time.Second
)

// defaultAnnotationTemplates is the out-of-the-box annotation set stamped onto
// mutated pods.
//
// This is a two-layer template pipeline:
//
//	Layer 2 (webhook): variables .PVName, .OwnerName, .PodName, .PodIndex,
//	.VolumeHandle are resolved at admission time.  Variables .PodUID and .PodNamespace are
//	pre-populated with their own template action text ("{{.PodUID}}" etc.) so
//	that they pass through unchanged as annotation values.
//
//	Layer 3 (pv-snapshotter): the annotation value is re-rendered at Mounts()
//	time; {{.PodUID}} is substituted with the actual pod UID.
//
// Template text uses plain {{.PodUID}} (no {{ "{{" }} escaping) because these
// defaults are registered as a pflag stringToString value.  pflag uses CSV
// parsing for that type, and bare double-quotes in a CSV field cause a parse
// error — so the {{ "{{" }} idiom cannot be used in flag values.
var defaultAnnotationTemplates = map[string]string{
	"pv-snapshotter.humble-mun.io/upperdir-path-template": "/var/lib/kubelet/pods/{{.PodUID}}/volumes/kubernetes.io~csi/{{.PVName}}/mount",
	"pv-snapshotter.humble-mun.io/var.PVName":             "{{.PVName}}",
}

// RegisterFlags registers all webhook flags with the provided flag set.
func RegisterFlags(pfs *pflag.FlagSet) {
	pfs.String(flagPVCNameTemplate, defaultPVCNameTemplate,
		"Go template rendered to the PVC name to bind to the pod. "+
			"Template variables: .OwnerName, .PodName, .PodIndex.")
	pfs.String(flagPVCSelectorTemplate, defaultPVCSelectorTemplate,
		"Go template rendered to a label selector string used to list PVCs. "+
			"Used only when --"+flagPVCNameTemplate+" produces an empty value.")
	pfs.Int(flagMaxOwnerDepth, defaultMaxOwnerDepth,
		"Maximum owner-reference traversal depth when resolving the controlling owner. "+
			"0 disables traversal (pod name is used as OwnerName).")
	pfs.String(flagDefaultRuntimeClass, defaultDefaultRuntimeClass,
		"Base RuntimeClass name used when the pod does not specify runtimeClassName. "+
			"The pv-backed name is formed by appending --"+flagRuntimeClassSuffix+". "+
			"When empty and the pod has no runtimeClassName, runtimeClassName is not patched.")
	pfs.String(flagRuntimeClassSuffix, defaultRuntimeClassSuffix,
		"Suffix appended to the pod's runtimeClassName (or --"+flagDefaultRuntimeClass+
			" when the pod has none) to select the pv-backed RuntimeClass.")
	pfs.StringToString(flagAnnotationTemplates, defaultAnnotationTemplates,
		"Map of annotation key=Go-template-value stamped onto mutated pods. "+
			"Template variables: .OwnerName, .PodName, .PodIndex, .PVName, .VolumeHandle.")
	pfs.String(flagStateMountPath, defaultStateMountPath,
		"Container mount path for the injected state volume on the primary container.")
	pfs.Duration(flagBoundTimeout, defaultBoundTimeout,
		"How long to wait for the PVC to reach the Bound phase before denying the pod. "+
			"pv-snapshotter cannot prepare the overlay upperdir on an unbound volume, so "+
			"allowing the pod through while the PVC is unbound would only defer the failure.")
	pfs.Bool(flagEnabled, defaultEnabled,
		"Enable the mutating admission webhook endpoint.")
}

// Enabled reports whether the webhook is configured to run.
func Enabled() bool {
	return viper.GetBool(flagEnabled)
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// Handler is a mutating admission webhook that:
//
//  1. Resolves the controlling owner of the pod by traversing owner references
//     up to maxOwnerDepth hops.
//  2. Looks up the PVC associated with that owner (by name template or label
//     selector template) and waits up to boundTimeout for it to reach Bound.
//  3. Injects a state volume plus a primary-container-only volumeMount backed
//     by that PVC and stamps pv-snapshotter annotations onto the pod, enabling
//     pv-backed overlay routing.
type Handler struct {
	logger                logr.Logger
	client                kubernetes.Interface
	dynamic               dynamic.Interface
	pvcNameTmpl           *template.Template
	pvcSelectorTmpl       *template.Template
	pvcNameTmplAnnotation string
	annotationTmpls       map[string]*template.Template
	maxOwnerDepth         int
	defaultRuntimeClass   string
	runtimeClassSuffix    string
	stateMountPath        string
	boundTimeout          time.Duration
}

// New constructs a Handler from viper-resolved flags.
func New(logger logr.Logger) (*Handler, error) {
	logger = logger.WithName("webhook")

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("building in-cluster REST config: %w", err)
	}

	kc, kerr := kubernetes.NewForConfig(cfg)
	if kerr != nil {
		return nil, fmt.Errorf("building Kubernetes typed client: %w", kerr)
	}

	dc, derr := dynamic.NewForConfig(cfg)
	if derr != nil {
		return nil, fmt.Errorf("building Kubernetes dynamic client: %w", derr)
	}

	pvcNameTmpl, terr := parseTemplate("pvc-name", viper.GetString(flagPVCNameTemplate))
	if terr != nil {
		return nil, fmt.Errorf("parsing --%s: %w", flagPVCNameTemplate, terr)
	}

	pvcSelectorTmpl, terr := parseTemplate("pvc-selector", viper.GetString(flagPVCSelectorTemplate))
	if terr != nil {
		return nil, fmt.Errorf("parsing --%s: %w", flagPVCSelectorTemplate, terr)
	}

	// The per-pod PVC-name override annotation key shares the configurable
	// --annotation-prefix with the snapshotter resolver; only the suffix is
	// fixed. This keeps a single prefix authoritative across the daemon.
	prefix, perr := annotation.ResolvePrefix()
	if perr != nil {
		return nil, perr
	}
	pvcNameTmplAnnotation := annotation.Key(prefix, annotationSuffixPVCNameTemplate)

	// Read annotation templates directly from the pflag layer via viper.
	// IMPORTANT: viper.GetStringMapString is safe here ONLY because
	// webhook-annotation-templates is never written to daemon.yaml (the viper
	// YAML config file).  viper's YAML path runs insensitiviseMap, which
	// lowercases every map key recursively — corrupting annotation key casing
	// (e.g. "var.PVName" → "var.pvname").  When the key is absent from the
	// YAML file, viper falls through to the pflag binding and calls
	// stringToStringConv, which preserves casing.  Do not add this key to
	// the ConfigMap / daemon.yaml under any circumstances.
	rawAnnotationTmpls := viper.GetStringMapString(flagAnnotationTemplates)
	annotationTmpls := make(map[string]*template.Template, len(rawAnnotationTmpls))
	for k, v := range rawAnnotationTmpls {
		t, aerr := parseTemplate("annotation/"+k, v)
		if aerr != nil {
			return nil, fmt.Errorf("parsing annotation template for key %q: %w", k, aerr)
		}
		annotationTmpls[k] = t
	}

	return &Handler{
		logger:                logger,
		client:                kc,
		dynamic:               dc,
		pvcNameTmpl:           pvcNameTmpl,
		pvcSelectorTmpl:       pvcSelectorTmpl,
		pvcNameTmplAnnotation: pvcNameTmplAnnotation,
		annotationTmpls:       annotationTmpls,
		maxOwnerDepth:         viper.GetInt(flagMaxOwnerDepth),
		defaultRuntimeClass:   viper.GetString(flagDefaultRuntimeClass),
		runtimeClassSuffix:    viper.GetString(flagRuntimeClassSuffix),
		stateMountPath:        viper.GetString(flagStateMountPath),
		boundTimeout:          viper.GetDuration(flagBoundTimeout),
	}, nil
}

// RegisterRoute registers the /mutate endpoint on the provided gin engine.
// The signature satisfies chassis/pkg/server.HTTPServer.RegisterRoute.
func (h *Handler) RegisterRoute(r *gin.Engine) {
	r.POST("/mutate", h.handle)
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

func (h *Handler) handle(c *gin.Context) {
	var review admissionv1.AdmissionReview
	if err := c.ShouldBindBodyWithJSON(&review); err != nil {
		h.logger.Error(err, "malformed AdmissionReview request")
		c.JSON(http.StatusBadRequest, apierrors.NewBadRequest(err.Error()).Status())
		return
	}
	if review.Request == nil {
		c.JSON(http.StatusBadRequest, apierrors.NewBadRequest("AdmissionReview.request is nil").Status())
		return
	}

	resp := h.mutate(c.Request.Context(), review.Request)
	c.JSON(http.StatusOK, admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: review.APIVersion,
			Kind:       review.Kind,
		},
		Response: resp,
	})
}

// ---------------------------------------------------------------------------
// Core mutation logic
// ---------------------------------------------------------------------------

// templateData carries the variables available in every rendered template.
//
// Layer-2 variables (.PVName, .VolumeHandle, .OwnerName, .PodName, .PodIndex)
// are resolved by the webhook and substituted at admission time.
//
// Layer-3 pass-through fields (.PodUID, .PodNamespace) are pre-populated with
// their own Go template action strings (e.g. PodUID = "{{.PodUID}}"). When the
// webhook executes an annotation template that references {{.PodUID}}, the
// output is the literal string "{{.PodUID}}" — which pv-snapshotter then
// re-renders at Mounts() time with the actual pod UID.
//
// This eliminates the need for the double-brace {{ "{{" }} escape syntax in
// annotation template values, which is incompatible with pflag's CSV-based
// stringToString flag format (CSV treats bare " as a parse error).
type templateData struct {
	// Layer-2: resolved by the webhook at admission time.
	OwnerName    string
	PodName      string
	PVName       string
	VolumeHandle string
	// PodIndex is the pod's ordinal/completion-index from the
	// "apps.kubernetes.io/pod-index" label (StatefulSet and Indexed Job
	// pods, Kubernetes 1.28+). Empty when the pod carries no such label.
	PodIndex string

	// Layer-3 pass-throughs: set to their own template action text so that
	// {{.PodUID}} in an annotation template renders as the literal string
	// "{{.PodUID}}" for pv-snapshotter to resolve at Mounts() time.
	PodUID       string // always "{{.PodUID}}"
	PodNamespace string // always "{{.PodNamespace}}"
}

func (h *Handler) mutate(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	log := h.logger.WithValues("name", req.Name, "namespace", req.Namespace)

	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		log.Error(err, "failed to decode pod")
		return deny(req.UID, fmt.Sprintf("decoding pod: %v", err))
	}

	log.V(4).Info("mutating pod")

	ownerName, err := h.resolveOwner(ctx, &pod, req.Namespace)
	if err != nil {
		log.Error(err, "failed to resolve controlling owner")
		return deny(req.UID, fmt.Sprintf("resolving controlling owner: %v", err))
	}
	log.V(4).Info("resolved controlling owner", "ownerName", ownerName)

	data := templateData{
		OwnerName: ownerName,
		PodName:   req.Name,
		PodIndex:  pod.Labels[podIndexLabel],
		// Pre-populate Layer-3 pass-through fields with their own action text.
		// Templates that reference {{.PodUID}} will output "{{.PodUID}}"
		// literally, which pv-snapshotter re-renders at Mounts() time.
		PodUID:       "{{.PodUID}}",
		PodNamespace: "{{.PodNamespace}}",
	}
	pvc, err := h.resolvePVC(ctx, req.Namespace, &pod, data)
	if err != nil {
		log.Error(err, "failed to resolve PVC", "ownerName", ownerName)
		return deny(req.UID, fmt.Sprintf("resolving PVC for owner %q: %v", ownerName, err))
	}
	log.V(4).Info("resolved PVC", "pvc", pvc.Name)

	// Wait for the PVC (and its backing PV) to be Bound before proceeding.
	// pv-snapshotter cannot set up the overlay upperdir on an unbound volume;
	// allowing the pod through early only defers the failure to the node.
	pvName, volumeHandle, err := h.waitBound(ctx, pvc, req.Namespace, log)
	if err != nil {
		log.Error(err, "PVC/PV not ready", "pvc", pvc.Name)
		return deny(req.UID, fmt.Sprintf("waiting for PVC %q to be bound: %v", pvc.Name, err))
	}
	log.V(4).Info("PVC bound", "pvc", pvc.Name, "pvName", pvName, "volumeHandle", volumeHandle)

	data.PVName = pvName
	data.VolumeHandle = volumeHandle

	patch, err := h.buildPatch(&pod, pvc.Name, data)
	if err != nil {
		log.Error(err, "failed to build patch")
		return deny(req.UID, fmt.Sprintf("building patch: %v", err))
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		log.Error(err, "failed to marshal patch")
		return deny(req.UID, fmt.Sprintf("marshaling patch: %v", err))
	}

	log.V(4).Info("mutation successful", "pvcName", pvc.Name, "ownerName", ownerName)
	patchType := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		UID:       req.UID,
		Allowed:   true,
		Patch:     patchBytes,
		PatchType: &patchType,
	}
}

// ---------------------------------------------------------------------------
// Owner resolution
// ---------------------------------------------------------------------------

// resolveOwner traverses the owner-reference chain up to h.maxOwnerDepth hops
// and returns the name of the deepest controlling owner found.
//
// At each level the controller owner reference (Controller=true) is followed.
// If multiple controller refs exist at the same level, the first is used and a
// warning is logged.
func (h *Handler) resolveOwner(ctx context.Context, pod *corev1.Pod, ns string) (string, error) {
	if h.maxOwnerDepth == 0 {
		return pod.Name, nil
	}

	currentName := pod.Name
	currentRefs := pod.OwnerReferences

	for depth := 0; depth < h.maxOwnerDepth; depth++ {
		controllers := controllerRefs(currentRefs)
		if len(controllers) == 0 {
			break
		}
		if len(controllers) > 1 {
			names := make([]string, len(controllers))
			for i, r := range controllers {
				names[i] = r.Kind + "/" + r.Name
			}
			h.logger.Info("multiple controller owner references; using first",
				"currentName", currentName, "depth", depth, "controllers", names)
		}
		ref := &controllers[0]

		gvr, err := ownerRefToGVR(ref)
		if err != nil {
			h.logger.V(4).Info("unrecognised owner kind; stopping traversal",
				"kind", ref.Kind, "name", ref.Name, "depth", depth)
			break
		}

		obj, err := h.dynamic.Resource(gvr).Namespace(ns).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("getting %s/%s (depth %d): %w", ref.Kind, ref.Name, depth, err)
		}

		currentName = obj.GetName()
		currentRefs = obj.GetOwnerReferences()
	}

	return currentName, nil
}

// controllerRefs returns all owner references that declare Controller=true.
func controllerRefs(refs []metav1.OwnerReference) []metav1.OwnerReference {
	var out []metav1.OwnerReference
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			out = append(out, refs[i])
		}
	}
	return out
}

// ownerRefToGVR maps a well-known owner Kind to its canonical GroupVersionResource.
// Returns an error for unrecognised kinds so traversal can stop gracefully.
func ownerRefToGVR(ref *metav1.OwnerReference) (schema.GroupVersionResource, error) {
	switch ref.Kind {
	case "ReplicaSet":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}, nil
	case "StatefulSet":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, nil
	case "Deployment":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, nil
	case "DaemonSet":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, nil
	case "Job":
		return schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, nil
	case "CronJob":
		return schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}, nil
	default:
		return schema.GroupVersionResource{}, fmt.Errorf("unrecognised owner kind %q", ref.Kind)
	}
}

// ---------------------------------------------------------------------------
// PVC resolution
// ---------------------------------------------------------------------------

// resolvePVC finds the PVC to bind to the pod.
//
// Resolution order:
//  1. Per-pod override: if the pod sets the pvcNameTmplAnnotation annotation,
//     render its value (same template variables as pvcNameTmpl) and fetch the
//     PVC by that name. This lets a pod select a PVC whose lifecycle is
//     independent of the pod/owner name; the annotation value may also be a
//     literal PVC name with no template actions. The override bypasses the
//     global name and selector templates, and a rendered-empty value is a hard
//     error rather than a fallback.
//  2. Render pvcNameTmpl; if non-empty, fetch the PVC by name (hard error if absent).
//  3. Fall back to pvcSelectorTmpl; render the selector, list PVCs, take the first
//     result (warning logged when multiple match).
//
// Returns an error when no PVC is found, causing the pod to be denied.
func (h *Handler) resolvePVC(ctx context.Context, ns string, pod *corev1.Pod, data templateData) (*corev1.PersistentVolumeClaim, error) {
	if h.pvcNameTmplAnnotation != "" {
		if tmplText, ok := pod.Annotations[h.pvcNameTmplAnnotation]; ok {
			return h.resolvePVCFromAnnotation(ctx, ns, tmplText, data)
		}
	}

	pvcName, err := renderTemplate(h.pvcNameTmpl, data)
	if err != nil {
		return nil, fmt.Errorf("rendering pvc-name-template: %w", err)
	}
	if pvcName != "" {
		pvc, gerr := h.client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pvcName, metav1.GetOptions{})
		if gerr != nil {
			return nil, fmt.Errorf("fetching PVC %q: %w", pvcName, gerr)
		}
		return pvc, nil
	}

	selectorStr, err := renderTemplate(h.pvcSelectorTmpl, data)
	if err != nil {
		return nil, fmt.Errorf("rendering pvc-selector-template: %w", err)
	}
	if selectorStr == "" {
		return nil, fmt.Errorf(
			"neither pvc-name-template nor pvc-selector-template produced a value (ownerName=%q, podName=%q)",
			data.OwnerName, data.PodName)
	}

	sel, err := labels.Parse(selectorStr)
	if err != nil {
		return nil, fmt.Errorf("parsing PVC label selector %q: %w", selectorStr, err)
	}

	list, err := h.client.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{
		LabelSelector:  sel.String(),
		TimeoutSeconds: new(int64((10 * time.Second).Seconds())),
	})
	if err != nil {
		return nil, fmt.Errorf("listing PVCs with selector %q: %w", selectorStr, err)
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("no PVC matched selector %q", selectorStr)
	}
	if len(list.Items) > 1 {
		names := make([]string, len(list.Items))
		for i := range list.Items {
			names[i] = list.Items[i].Name
		}
		h.logger.Info("multiple PVCs matched selector; using first",
			"selector", selectorStr, "pvcs", names)
	}
	return &list.Items[0], nil
}

// resolvePVCFromAnnotation renders a per-pod PVC-name template (the value of the
// pvcNameTmplAnnotation annotation) and fetches the named PVC. The template is
// compiled per request because its text is supplied by the pod rather than by
// flags. An empty render is rejected: when a pod opts into the override, falling
// back to the global templates would silently ignore the pod's intent.
func (h *Handler) resolvePVCFromAnnotation(ctx context.Context, ns, tmplText string, data templateData) (*corev1.PersistentVolumeClaim, error) {
	tmpl, err := parseTemplate("pvc-name-annotation", tmplText)
	if err != nil {
		return nil, fmt.Errorf("parsing pod annotation %q as template: %w", h.pvcNameTmplAnnotation, err)
	}
	pvcName, err := renderTemplate(tmpl, data)
	if err != nil {
		return nil, fmt.Errorf("rendering pod annotation %q: %w", h.pvcNameTmplAnnotation, err)
	}
	if pvcName == "" {
		return nil, fmt.Errorf("pod annotation %q rendered an empty PVC name", h.pvcNameTmplAnnotation)
	}
	pvc, err := h.client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("fetching PVC %q (from pod annotation %q): %w", pvcName, h.pvcNameTmplAnnotation, err)
	}
	return pvc, nil
}

// ---------------------------------------------------------------------------
// PVC / PV bound wait
// ---------------------------------------------------------------------------

// waitBound polls the PVC until it reaches the Bound phase (or the configured
// timeout expires), then fetches the bound PV.
//
// Rationale: pv-snapshotter cannot create the overlay upperdir on an unbound
// volume — refusing the pod here is cleaner than deferring the failure to the
// node. A timeout is used rather than Watch because webhook handlers run with
// a hard API-server deadline that is much shorter than most Watch use cases.
func (h *Handler) waitBound(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
	ns string,
	log logr.Logger,
) (pvName, volumeHandle string, err error) {

	deadline := time.Now().Add(h.boundTimeout)

	for pvc.Status.Phase != corev1.ClaimBound {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", "", fmt.Errorf(
				"timed out after %s waiting for PVC %q to be bound (current phase: %s)",
				h.boundTimeout, pvc.Name, pvc.Status.Phase)
		}

		sleep := boundPollInterval
		if sleep > remaining {
			sleep = remaining
		}
		log.V(4).Info("PVC not yet bound; retrying",
			"pvc", pvc.Name, "phase", pvc.Status.Phase, "remaining", remaining.Round(time.Second))

		select {
		case <-ctx.Done():
			return "", "", fmt.Errorf("context cancelled while waiting for PVC %q: %w", pvc.Name, ctx.Err())
		case <-time.After(sleep):
		}

		pvc, err = h.client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pvc.Name, metav1.GetOptions{})
		if err != nil {
			return "", "", fmt.Errorf("re-fetching PVC %q: %w", pvc.Name, err)
		}
	}

	pvName = pvc.Spec.VolumeName
	if pvName == "" {
		return "", "", fmt.Errorf("PVC %q is Bound but spec.volumeName is empty", pvc.Name)
	}

	pv, gerr := h.client.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if gerr != nil {
		return "", "", fmt.Errorf("fetching PV %q: %w", pvName, gerr)
	}

	if pv.Spec.CSI != nil {
		volumeHandle = pv.Spec.CSI.VolumeHandle
	}
	return pvName, volumeHandle, nil
}

// ---------------------------------------------------------------------------
// Patch construction
// ---------------------------------------------------------------------------

// jsonPatchOp is a single RFC 6902 JSON Patch operation.
type jsonPatchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// buildPatch assembles the RFC 6902 JSON Patch operations that mutate the pod:
//
//  1. Stamp pv-snapshotter annotations (key → rendered template value).
//  2. Append the state volume backed by the resolved PVC.
//  3. Add the state volumeMount only to the primary container
//     (spec.containers[0]) so kubelet mounts the PVC without exposing the raw
//     overlay upperdir to sidecars or init containers.
//  4. Rewrite runtimeClassName to the pv-backed variant (appends the suffix).
func (h *Handler) buildPatch(pod *corev1.Pod, pvcName string, data templateData) ([]jsonPatchOp, error) {
	var ops []jsonPatchOp

	// Ensure the annotations map exists before adding individual keys.
	if pod.Annotations == nil {
		ops = append(ops, jsonPatchOp{
			Op:    "add",
			Path:  "/metadata/annotations",
			Value: map[string]string{},
		})
	}
	for key, tmpl := range h.annotationTmpls {
		val, err := renderTemplate(tmpl, data)
		if err != nil {
			return nil, fmt.Errorf("rendering annotation template for key %q: %w", key, err)
		}
		// JSON Pointer escaping per RFC 6901: '~' → '~0', '/' → '~1'.
		jsonKey := strings.ReplaceAll(key, "~", "~0")
		jsonKey = strings.ReplaceAll(jsonKey, "/", "~1")
		ops = append(ops, jsonPatchOp{
			Op:    "add",
			Path:  "/metadata/annotations/" + jsonKey,
			Value: val,
		})
	}

	stateVolume := corev1.Volume{
		Name: statePVCVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvcName,
			},
		},
	}
	if pod.Spec.Volumes == nil {
		ops = append(ops, jsonPatchOp{Op: "add", Path: "/spec/volumes", Value: []interface{}{}})
	}
	ops = append(ops, jsonPatchOp{Op: "add", Path: "/spec/volumes/-", Value: stateVolume})

	stateMount := corev1.VolumeMount{
		Name:      statePVCVolumeName,
		MountPath: h.stateMountPath,
	}
	if len(pod.Spec.Containers) > 0 {
		if pod.Spec.Containers[0].VolumeMounts == nil {
			ops = append(ops, jsonPatchOp{
				Op:    "add",
				Path:  "/spec/containers/0/volumeMounts",
				Value: []corev1.VolumeMount{stateMount},
			})
		} else {
			ops = append(ops, jsonPatchOp{
				Op:    "add",
				Path:  "/spec/containers/0/volumeMounts/-",
				Value: stateMount,
			})
		}
	}

	// ── runtimeClassName ─────────────────────────────────────────────────────
	// Determine the base RuntimeClass:
	//   - Pod already specifies one → use it as-is (append suffix).
	//   - Pod has none → fall back to defaultRuntimeClass (append suffix).
	//   - Both are empty → leave runtimeClassName unset.
	baseRC := ""
	if pod.Spec.RuntimeClassName != nil {
		baseRC = *pod.Spec.RuntimeClassName
	}
	if baseRC == "" {
		baseRC = h.defaultRuntimeClass
	}
	if baseRC != "" {
		pvRC := baseRC + h.runtimeClassSuffix
		if pod.Spec.RuntimeClassName == nil {
			// Field does not exist yet in the object; use "add".
			ops = append(ops, jsonPatchOp{
				Op:    "add",
				Path:  "/spec/runtimeClassName",
				Value: pvRC,
			})
		} else {
			ops = append(ops, jsonPatchOp{
				Op:    "replace",
				Path:  "/spec/runtimeClassName",
				Value: pvRC,
			})
		}
	}

	return ops, nil
}

// ---------------------------------------------------------------------------
// Template helpers
// ---------------------------------------------------------------------------

// templateFuncs are the custom functions available in every template.
var templateFuncs = template.FuncMap{
	// sha256 returns the lowercase hex-encoded SHA-256 digest of the input.
	// Useful for Ceph RBD volume-handle path segments:
	//   {{ .VolumeHandle | sha256 }}
	"sha256": func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return fmt.Sprintf("%x", sum)
	},
}

// parseTemplate compiles a named template with the custom func map.
func parseTemplate(name, text string) (*template.Template, error) {
	return template.New(name).Funcs(templateFuncs).Parse(text)
}

// renderTemplate executes tmpl with data and returns the rendered string.
func renderTemplate(tmpl *template.Template, data templateData) (string, error) {
	if tmpl == nil {
		return "", nil
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ---------------------------------------------------------------------------
// AdmissionResponse helpers
// ---------------------------------------------------------------------------

// deny returns a rejecting AdmissionResponse with an HTTP 422 status.
func deny(uid types.UID, msg string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		UID:     uid,
		Allowed: false,
		Result: &metav1.Status{
			Code:    http.StatusUnprocessableEntity,
			Message: msg,
		},
	}
}
