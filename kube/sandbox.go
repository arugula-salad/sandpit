package kube

import (
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/arugula-salad/sandpit/internal/store"
)

// sandboxGVR is agent-sandbox's core resource (v1.0 and later).
var sandboxGVR = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}

const (
	modeRunning   = "Running"
	modeSuspended = "Suspended"

	// Labels on everything the engine makes; the pod watch selects on managedBy.
	managedBy = "app.kubernetes.io/managed-by"
	labelID   = "sandpit.dev/id"
	labelAPI  = "sandpit.dev/api"

	volumeName    = "persist"
	containerName = "sandbox"
	tokenKey      = "token"
)

func (e *Engine) labels(rec store.Record) map[string]string {
	api := rec.API
	if api == "" {
		api = "sprites"
	}
	return map[string]string{managedBy: "sandpit", labelID: rec.ID, labelAPI: api}
}

func secretName(id string) string { return sandboxName(id) + "-agent" }

// sandboxObject is the Sandbox for rec: one container, the guest image, with
// sandpit-agent as its entrypoint (the image's), sized by rec.Config, and
// a volume on Options.PersistPath.
func (e *Engine) sandboxObject(rec store.Record) *unstructured.Unstructured {
	port := int32(e.opts.AgentPort)
	c := corev1.Container{
		Name:            containerName,
		Image:           e.opts.Image,
		ImagePullPolicy: e.opts.ImagePullPolicy,
		Args:            []string{"serve", "--system-services", "--listen", "tcp:0.0.0.0:" + strconv.Itoa(e.opts.AgentPort)},
		Env: []corev1.EnvVar{{Name: "SANDPIT_AGENT_TOKEN", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secretName(rec.ID)}, Key: tokenKey}}}},
		Ports: []corev1.ContainerPort{{Name: "agent", ContainerPort: port}},
		// Ready is listening: whether it answers the token is waitAgent's to find out.
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}},
			PeriodSeconds: 1, FailureThreshold: 3},
		Resources: resourcesOf(rec.Config),
	}
	pod := corev1.PodSpec{
		Hostname:                     rec.Hostname,
		AutomountServiceAccountToken: ptr(false), // the sandbox gets no say in the cluster
		EnableServiceLinks:           ptr(false),
		RestartPolicy:                corev1.RestartPolicyAlways,
		Containers:                   []corev1.Container{c},
	}
	if e.opts.RuntimeClass != "" {
		pod.RuntimeClassName = &e.opts.RuntimeClass
	}
	spec := map[string]any{"operatingMode": modeRunning}
	if p := e.opts.PersistPath; p != "" {
		pod.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: volumeName, MountPath: p}}
		// A new volume hides what the image has at p (a home directory's
		// dotfiles, owned by its user): copy that in once, on the first start.
		pod.InitContainers = []corev1.Container{{
			Name: "seed", Image: e.opts.Image, ImagePullPolicy: e.opts.ImagePullPolicy,
			Command:      []string{"/bin/sh", "-c", `[ -e /seed/.sandpit-seeded ] || { cp -a "$1"/. /seed/ && touch /seed/.sandpit-seeded; }`, "seed", p},
			VolumeMounts: []corev1.VolumeMount{{Name: volumeName, MountPath: "/seed"}},
			// Sized as the sandbox is, or the pod would not be Guaranteed (below).
			Resources: c.Resources,
		}}
		pvc := corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: volumeName, Labels: e.labels(rec)},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(e.opts.DiskSize)}},
			},
		}
		if e.opts.StorageClass != "" {
			pvc.Spec.StorageClassName = &e.opts.StorageClass
		}
		claim := toMap(&pvc)
		delete(claim, "status") // a zero one, which the API server warns about
		spec["volumeClaimTemplates"] = []any{claim}
	}
	spec["podTemplate"] = map[string]any{"metadata": map[string]any{"labels": toAny(e.labels(rec))}, "spec": toMap(&pod)}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": sandboxGVR.GroupVersion().String(),
		"kind":       "Sandbox",
		"metadata":   map[string]any{"name": sandboxName(rec.ID), "namespace": e.opts.Namespace, "labels": toAny(e.labels(rec))},
		"spec":       spec,
	}}
}

// tokenSecret holds rec's agent token, owned by its Sandbox (uid).
func (e *Engine) tokenSecret(rec store.Record, uid types.UID) *corev1.Secret {
	x, _ := extOf(rec)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName(rec.ID), Namespace: e.opts.Namespace, Labels: e.labels(rec),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: sandboxGVR.GroupVersion().String(), Kind: "Sandbox",
				Name: sandboxName(rec.ID), UID: uid, BlockOwnerDeletion: ptr(true)}}},
		StringData: map[string]string{tokenKey: x.Token},
	}
}

// resourcesOf asks for what the record is configured with, as both request
// and limit: a sandbox is sized like a VM, not burstable. That also makes the
// pod Guaranteed, whose oom_score_adj (-997) is one a process may raise: E2B's
// envd starts every command by writing 100 to its own, which in a Burstable
// pod (996, say) is a lowering, refused without CAP_SYS_RESOURCE, and every
// command fails with "echo: I/O error".
func resourcesOf(c store.Config) corev1.ResourceRequirements {
	rl := corev1.ResourceList{}
	if c.CPUs > 0 {
		rl[corev1.ResourceCPU] = resource.MustParse(strconv.Itoa(c.CPUs))
	}
	if c.RamMB > 0 {
		rl[corev1.ResourceMemory] = resource.MustParse(fmt.Sprintf("%dMi", c.RamMB))
	}
	if len(rl) == 0 {
		return corev1.ResourceRequirements{}
	}
	return corev1.ResourceRequirements{Requests: rl, Limits: rl}
}

func toMap(v any) map[string]any {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(v)
	if err != nil {
		panic(err) // only our own typed objects come here
	}
	return m
}

func toAny(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func ptr[T any](v T) *T { return &v }
