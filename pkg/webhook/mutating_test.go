package webhook

import (
	"testing"
	"text/template"

	corev1 "k8s.io/api/core/v1"
)

func TestBuildPatchAddsStateMountOnlyToPrimaryContainer(t *testing.T) {
	handler := &Handler{
		annotationTmpls:     map[string]*template.Template{},
		defaultRuntimeClass: "runc",
		runtimeClassSuffix:  "-pv",
		stateMountPath:      "/.platform/state",
	}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main"},
				{
					Name: "sidecar",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "existing", MountPath: "/existing"},
					},
				},
			},
			InitContainers: []corev1.Container{
				{Name: "init"},
			},
		},
	}

	ops, err := handler.buildPatch(pod, "state-pvc", templateData{})
	if err != nil {
		t.Fatalf("buildPatch returned error: %v", err)
	}

	mountOps := filterOps(ops, func(op jsonPatchOp) bool {
		return op.Path == "/spec/containers/0/volumeMounts" ||
			op.Path == "/spec/containers/0/volumeMounts/-" ||
			op.Path == "/spec/containers/1/volumeMounts/-" ||
			op.Path == "/spec/initContainers/0/volumeMounts/-"
	})
	if len(mountOps) != 1 {
		t.Fatalf("expected exactly one state volumeMount patch, got %d: %#v", len(mountOps), mountOps)
	}
	if mountOps[0].Path != "/spec/containers/0/volumeMounts" {
		t.Fatalf("expected state mount to initialize primary container volumeMounts, got path %q", mountOps[0].Path)
	}
	mounts, ok := mountOps[0].Value.([]corev1.VolumeMount)
	if !ok {
		t.Fatalf("expected initialized volumeMounts slice, got %T", mountOps[0].Value)
	}
	if len(mounts) != 1 {
		t.Fatalf("expected one initialized volumeMount, got %d", len(mounts))
	}
	assertStateMount(t, mounts[0])
}

func TestBuildPatchAppendsStateMountToExistingPrimaryContainerMounts(t *testing.T) {
	handler := &Handler{
		annotationTmpls:     map[string]*template.Template{},
		defaultRuntimeClass: "runc",
		runtimeClassSuffix:  "-pv",
		stateMountPath:      "/.platform/state",
	}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "existing", MountPath: "/existing"},
					},
				},
				{Name: "sidecar"},
			},
		},
	}

	ops, err := handler.buildPatch(pod, "state-pvc", templateData{})
	if err != nil {
		t.Fatalf("buildPatch returned error: %v", err)
	}

	mountOps := filterOps(ops, func(op jsonPatchOp) bool {
		return op.Path == "/spec/containers/0/volumeMounts/-" ||
			op.Path == "/spec/containers/1/volumeMounts" ||
			op.Path == "/spec/containers/1/volumeMounts/-"
	})
	if len(mountOps) != 1 {
		t.Fatalf("expected exactly one state volumeMount patch, got %d: %#v", len(mountOps), mountOps)
	}
	if mountOps[0].Path != "/spec/containers/0/volumeMounts/-" {
		t.Fatalf("expected state mount to append to primary container volumeMounts, got path %q", mountOps[0].Path)
	}
	mount, ok := mountOps[0].Value.(corev1.VolumeMount)
	if !ok {
		t.Fatalf("expected appended volumeMount value, got %T", mountOps[0].Value)
	}
	assertStateMount(t, mount)
}

func filterOps(ops []jsonPatchOp, keep func(jsonPatchOp) bool) []jsonPatchOp {
	var out []jsonPatchOp
	for _, op := range ops {
		if keep(op) {
			out = append(out, op)
		}
	}
	return out
}

func assertStateMount(t *testing.T, mount corev1.VolumeMount) {
	t.Helper()
	if mount.Name != statePVCVolumeName {
		t.Fatalf("expected state mount name %q, got %q", statePVCVolumeName, mount.Name)
	}
	if mount.MountPath != "/.platform/state" {
		t.Fatalf("expected state mount path %q, got %q", "/.platform/state", mount.MountPath)
	}
}
