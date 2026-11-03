package environments

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	sandboxv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxfake "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
)

func newTestManager() *Manager {
	core := corefake.NewSimpleClientset()
	sb := sandboxfake.NewSimpleClientset()
	reconcile := func(obj *sandboxv1.Sandbox) {
		obj.UID = "sandbox-uid"
		controlled := true
		owner := []metav1.OwnerReference{{APIVersion: sandboxv1.GroupVersion.String(), Kind: "Sandbox", Name: obj.Name, UID: obj.UID, Controller: &controlled}}
		_ = core.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), Namespace, obj.Name)
		if obj.Spec.OperatingMode == sandboxv1.SandboxOperatingModeSuspended {
			obj.Status.Conditions = []metav1.Condition{{Type: "Suspended", Status: metav1.ConditionTrue, ObservedGeneration: obj.Generation}}
		} else {
			obj.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: obj.Generation}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: obj.Name, Namespace: Namespace, OwnerReferences: owner}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
			if err := core.Tracker().Add(pod); err != nil {
				panic(err)
			}
		}
		if obj.Spec.Service != nil && *obj.Spec.Service {
			h := fnv.New32a()
			_, _ = h.Write([]byte(obj.Name))
			svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: obj.Name, Namespace: Namespace, OwnerReferences: owner}, Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Selector: map[string]string{trackingLabel: fmt.Sprintf("%08x", h.Sum32())}}}
			_ = core.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("services"), Namespace, obj.Name)
			if err := core.Tracker().Add(svc); err != nil {
				panic(err)
			}
		}
	}
	sb.PrependReactor("create", "sandboxes", func(action ktesting.Action) (bool, runtime.Object, error) {
		reconcile(action.(ktesting.CreateAction).GetObject().(*sandboxv1.Sandbox))
		return false, nil, nil
	})
	sb.PrependReactor("update", "sandboxes", func(action ktesting.Action) (bool, runtime.Object, error) {
		reconcile(action.(ktesting.UpdateAction).GetObject().(*sandboxv1.Sandbox))
		return false, nil, nil
	})
	sb.PrependReactor("delete", "sandboxes", func(action ktesting.Action) (bool, runtime.Object, error) {
		if err := core.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), Namespace, action.(ktesting.DeleteAction).GetName()); err != nil && !apierrors.IsNotFound(err) {
			panic(err)
		}
		_ = core.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("services"), Namespace, action.(ktesting.DeleteAction).GetName())
		return false, nil, nil
	})
	return &Manager{Core: core, Sandboxes: sb, ClusterUID: "cluster-uid", Image: "agent-sandbox-go-dev:test", PollInterval: time.Millisecond}
}
func mustCreate(t *testing.T, m *Manager, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Create(ctx, name); err != nil {
		t.Fatal(err)
	}
}
func TestCreateIsolatesAndReusesCredentialsAndStorage(t *testing.T) {
	m := newTestManager()
	ctx := context.Background()
	mustCreate(t, m, "alpha")
	mustCreate(t, m, "beta")
	a, err := m.Password(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Password(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 || a == b {
		t.Fatal("senhas não são independentes")
	}
	before, err := m.Core.CoreV1().PersistentVolumeClaims(Namespace).Get(ctx, base("alpha")+"-home", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before.Annotations["test-marker"] = "preserve"
	if _, err = m.Core.CoreV1().PersistentVolumeClaims(Namespace).Update(ctx, before, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, m, "alpha")
	after, _ := m.Password(ctx, "alpha")
	if after != a {
		t.Fatal("senha mudou na repetição")
	}
	pvc, _ := m.Core.CoreV1().PersistentVolumeClaims(Namespace).Get(ctx, before.Name, metav1.GetOptions{})
	if pvc.Annotations["test-marker"] != "preserve" {
		t.Fatal("PVC alterado")
	}
	for _, name := range []string{"alpha", "beta"} {
		sb, err := m.Sandboxes.AgentsV1beta1().Sandboxes(Namespace).Get(ctx, base(name), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if sb.Spec.PodTemplate.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != base(name)+"-home" {
			t.Fatal("volume compartilhado")
		}
		if sb.Spec.PodTemplate.Spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Name != base(name)+"-auth" {
			t.Fatal("secret compartilhado")
		}
	}
}
func TestCreateRejectsCollisionsBeforeAnyWrite(t *testing.T) {
	for _, kind := range []string{"secret", "pvc", "sandbox", "service", "pod"} {
		t.Run(kind, func(t *testing.T) {
			m := newTestManager()
			ctx := context.Background()
			sb, svc, pvc, err := m.desired("alpha")
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "secret":
				_, err = m.Core.CoreV1().Secrets(Namespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: base("alpha") + "-auth"}}, metav1.CreateOptions{})
			case "pvc":
				pvc.Labels = nil
				_, err = m.Core.CoreV1().PersistentVolumeClaims(Namespace).Create(ctx, pvc, metav1.CreateOptions{})
			case "sandbox":
				sb.Annotations[ClusterAnnotation] = "other-cluster"
				_, err = m.Sandboxes.AgentsV1beta1().Sandboxes(Namespace).Create(ctx, sb, metav1.CreateOptions{})
			case "service":
				svc.Labels = nil
				_, err = m.Core.CoreV1().Services(Namespace).Create(ctx, svc, metav1.CreateOptions{})
			case "pod":
				_, err = m.Core.CoreV1().Pods(Namespace).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: base("alpha")}}, metav1.CreateOptions{})
			}
			if err != nil {
				t.Fatal(err)
			}
			core := m.Core.(*corefake.Clientset)
			sbs := m.Sandboxes.(*sandboxfake.Clientset)
			core.ClearActions()
			sbs.ClearActions()
			if err = m.Create(ctx, "alpha"); err == nil {
				t.Fatal("colisão aceita")
			}
			for _, a := range append(core.Actions(), sbs.Actions()...) {
				if a.GetVerb() != "get" {
					t.Fatalf("houve escrita antes da colisão: %s", a.GetVerb())
				}
			}
		})
	}
}
func TestCreateRejectsUnownedSecretRace(t *testing.T) {
	m := newTestManager()
	core := m.Core.(*corefake.Clientset)
	core.PrependReactor("create", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
		obj := action.(ktesting.CreateAction).GetObject().(*corev1.Secret).DeepCopy()
		obj.Labels = nil
		if err := core.Tracker().Add(obj); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, obj.Name)
	})
	if err := m.Create(context.Background(), "alpha"); err == nil {
		t.Fatal("corrida adotou Secret não gerenciado")
	}
	pvcs, _ := m.Core.CoreV1().PersistentVolumeClaims(Namespace).List(context.Background(), metav1.ListOptions{})
	if len(pvcs.Items) != 0 {
		t.Fatal("criação prosseguiu após colisão")
	}
}
func TestDeletePreservesDataRecreatesAndPurgesOnlySelectedEnvironment(t *testing.T) {
	m := newTestManager()
	ctx := context.Background()
	mustCreate(t, m, "alpha")
	mustCreate(t, m, "beta")
	password, _ := m.Password(ctx, "alpha")
	if err := m.Delete(ctx, "alpha", false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Core.CoreV1().PersistentVolumeClaims(Namespace).Get(ctx, base("alpha")+"-home", metav1.GetOptions{}); err != nil {
		t.Fatal("PVC não preservado:", err)
	}
	if got, err := m.Password(ctx, "alpha"); err != nil || got != password {
		t.Fatal("Secret não preservado")
	}
	mustCreate(t, m, "alpha")
	if got, _ := m.Password(ctx, "alpha"); got != password {
		t.Fatal("recriação alterou senha")
	}
	if err := m.Delete(ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Password(ctx, "alpha"); !apierrors.IsNotFound(err) {
		t.Fatal("Secret não removido")
	}
	if _, err := m.Core.CoreV1().PersistentVolumeClaims(Namespace).Get(ctx, base("alpha")+"-home", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("PVC não removido")
	}
	if _, err := m.Password(ctx, "beta"); err != nil {
		t.Fatal("outro ambiente afetado:", err)
	}
	if err := m.Delete(ctx, "alpha", true); err != nil {
		t.Fatal("delete não idempotente:", err)
	}
}
func TestDeleteRejectsAnyUnownedResourceBeforeDeleting(t *testing.T) {
	m := newTestManager()
	ctx := context.Background()
	mustCreate(t, m, "alpha")
	pvc, _ := m.Core.CoreV1().PersistentVolumeClaims(Namespace).Get(ctx, base("alpha")+"-home", metav1.GetOptions{})
	pvc.Labels = nil
	_, err := m.Core.CoreV1().PersistentVolumeClaims(Namespace).Update(ctx, pvc, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Delete(ctx, "alpha", true); err == nil {
		t.Fatal("purge aceitou PVC não gerenciado")
	}
	if _, err = m.Sandboxes.AgentsV1beta1().Sandboxes(Namespace).Get(ctx, base("alpha"), metav1.GetOptions{}); err != nil {
		t.Fatal("Sandbox removido antes da checagem")
	}
}
func TestCreateTimeoutPreservesPartialResources(t *testing.T) {
	m := newTestManager()
	m.Sandboxes.(*sandboxfake.Clientset).PrependReactor("create", "sandboxes", func(action ktesting.Action) (bool, runtime.Object, error) {
		obj := action.(ktesting.CreateAction).GetObject().(*sandboxv1.Sandbox)
		if err := m.Sandboxes.(*sandboxfake.Clientset).Tracker().Create(sandboxv1.GroupVersion.WithResource("sandboxes"), obj, Namespace); err != nil {
			t.Fatal(err)
		}
		return true, obj, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.Create(ctx, "alpha"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("esperado timeout, recebido %v", err)
	}
	if _, err := m.Password(context.Background(), "alpha"); err != nil {
		t.Fatal("Secret parcial perdido")
	}
}
func TestListFiltersOwnershipAndNeverIncludesOtherEnvironments(t *testing.T) {
	m := newTestManager()
	mustCreate(t, m, "alpha")
	ctx := context.Background()
	sb, _, _, _ := m.desired("foreign")
	sb.Annotations[ClusterAnnotation] = "other"
	if _, err := m.Sandboxes.AgentsV1beta1().Sandboxes(Namespace).Create(ctx, sb, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	entries, err := m.List(ctx)
	if err != nil || len(entries) != 1 || entries[0].Name != "alpha" || entries[0].Status != "Running" {
		t.Fatalf("lista incorreta: %+v, %v", entries, err)
	}
}
func TestParallelBoundsConcurrencyAndRetainsErrors(t *testing.T) {
	var active, maximum atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	done := make(chan []error, 1)
	go func() {
		done <- RunParallel(context.Background(), []string{"a", "b", "c"}, 2, func(_ context.Context, name string) error {
			n := active.Add(1)
			for {
				old := maximum.Load()
				if n <= old || maximum.CompareAndSwap(old, n) {
					break
				}
			}
			started <- struct{}{}
			<-release
			active.Add(-1)
			if name == "b" {
				return errors.New("expected")
			}
			return nil
		})
	}()
	<-started
	<-started
	if maximum.Load() != 2 {
		t.Fatal("paralelismo não aplicado")
	}
	close(release)
	errs := <-done
	if maximum.Load() > 2 || errs[0] != nil || errs[1] == nil || errs[2] != nil {
		t.Fatalf("resultado inválido: %v", errs)
	}
}

func TestCreateRejectsChangedSpecAndUnreadyPod(t *testing.T) {
	t.Run("changed spec", func(t *testing.T) {
		m := newTestManager()
		mustCreate(t, m, "alpha")
		ctx := context.Background()
		sb, err := m.Sandboxes.AgentsV1beta1().Sandboxes(Namespace).Get(ctx, base("alpha"), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		sb.Spec.PodTemplate.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "go-dev-home"
		if _, err = m.Sandboxes.AgentsV1beta1().Sandboxes(Namespace).Update(ctx, sb, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err = m.Create(ctx, "alpha"); err == nil {
			t.Fatal("spec alterada aceita apesar da annotation antiga")
		}
	})
	t.Run("stale Ready condition", func(t *testing.T) {
		m := newTestManager()
		mustCreate(t, m, "alpha")
		ctx := context.Background()
		pod, err := m.Core.CoreV1().Pods(Namespace).Get(ctx, base("alpha"), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		pod.Status.Conditions = nil
		if _, err = m.Core.CoreV1().Pods(Namespace).Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		limited, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		if err = m.Create(limited, "alpha"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deveria aguardar Pod Ready, recebido %v", err)
		}
	})
}
