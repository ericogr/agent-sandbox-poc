package environments

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corefake "k8s.io/client-go/kubernetes/fake"
	sandboxv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxfake "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
)

func TestLifecyclePreservesDataModeAndDeadline(t *testing.T) {
	m := newTestManager()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	before := time.Now()
	if err := m.CreateWithTTL(ctx, "alpha", time.Hour); err != nil {
		t.Fatal(err)
	}
	sb, _ := m.getSandbox(ctx, "alpha")
	deadline := sb.Spec.ShutdownTime.DeepCopy()
	if deadline.Before(&metav1.Time{Time: before.Add(time.Hour)}) || deadline.After(time.Now().Add(time.Hour)) {
		t.Fatal("TTL não começou na criação")
	}
	if sb.Spec.ShutdownPolicy == nil || *sb.Spec.ShutdownPolicy != sandboxv1.ShutdownPolicyRetain || sb.Spec.Service == nil || !*sb.Spec.Service {
		t.Fatal("políticas inesperadas")
	}
	svc, err := m.automaticService(ctx, "alpha", sb)
	if err != nil || svc == nil || len(svc.Labels) != 0 {
		t.Fatal("Service controller-owned sem labelsCLI rejeitado:", err)
	}
	for _, a := range m.Core.(*corefake.Clientset).Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "services" {
			t.Fatal("CLI criou Service")
		}
	}
	password, _ := m.Password(ctx, "alpha")
	if err := m.Stop(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(ctx, "alpha"); err != nil {
		t.Fatal("stop não idempotente:", err)
	}
	if err := m.CreateWithTTL(ctx, "alpha", 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	sb, _ = m.getSandbox(ctx, "alpha")
	if sb.Spec.OperatingMode != sandboxv1.SandboxOperatingModeSuspended || !sb.Spec.ShutdownTime.Equal(deadline) {
		t.Fatal("create retomou ou renovou prazo")
	}
	if pod, err := m.getPod(ctx, "alpha", sb); err != nil || pod != nil {
		t.Fatal("Pod sobreviveu stop", err)
	}
	entries, err := m.List(ctx)
	if err != nil || entries[0].Status != "Suspended" || entries[0].Expires == "-" {
		t.Fatalf("status incorreto %+v %v", entries, err)
	}
	if err := m.Start(ctx, "alpha", 0); err != nil {
		t.Fatal(err)
	}
	sb, _ = m.getSandbox(ctx, "alpha")
	if sb.Spec.OperatingMode != sandboxv1.SandboxOperatingModeRunning || sb.Spec.ShutdownTime != nil {
		t.Fatal("start não limpou prazo ao retomar")
	}
	if err := m.Start(ctx, "alpha", time.Hour); err != nil {
		t.Fatal(err)
	}
	sb, _ = m.getSandbox(ctx, "alpha")
	deadline = sb.Spec.ShutdownTime.DeepCopy()
	if err := m.Start(ctx, "alpha", 0); err != nil {
		t.Fatal(err)
	}
	sb, _ = m.getSandbox(ctx, "alpha")
	if !sb.Spec.ShutdownTime.Equal(deadline) {
		t.Fatal("start ativo semTTL renovou prazo")
	}
	if err := m.Create(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	sb, _ = m.getSandbox(ctx, "alpha")
	if !sb.Spec.ShutdownTime.Equal(deadline) {
		t.Fatal("create removeuTTL")
	}
	if got, err := m.Password(ctx, "alpha"); err != nil || got != password {
		t.Fatal("senha perdida")
	}
	if _, err := m.Core.CoreV1().PersistentVolumeClaims(Namespace).Get(ctx, base("alpha")+"-home", metav1.GetOptions{}); err != nil {
		t.Fatal("PVC perdido")
	}
}

func TestStartExpiredClearsOldDeadlineAndCreateDoesNotRestart(t *testing.T) {
	m := newTestManager()
	ctx := context.Background()
	mustCreate(t, m, "alpha")
	sb, _ := m.getSandbox(ctx, "alpha")
	past := metav1.NewTime(time.Now().Add(-time.Minute))
	sb.Spec.ShutdownTime = &past
	sb.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: sandboxv1.SandboxReasonExpired}}
	client := m.Sandboxes.(*sandboxfake.Clientset)
	if err := client.Tracker().Update(sandboxv1.GroupVersion.WithResource("sandboxes"), sb, Namespace); err != nil {
		t.Fatal(err)
	}
	if err := m.Core.CoreV1().Pods(Namespace).Delete(ctx, sb.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Core.CoreV1().Services(Namespace).Delete(ctx, sb.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateWithTTL(ctx, "alpha", time.Hour); err != nil {
		t.Fatal(err)
	}
	sb, _ = m.getSandbox(ctx, "alpha")
	if !sb.Spec.ShutdownTime.Equal(&past) {
		t.Fatal("create renovou expirado")
	}
	entries, _ := m.List(ctx)
	if entries[0].Status != "Expired" {
		t.Fatal(entries)
	}
	if err := m.Stop(ctx, "alpha"); err != nil {
		t.Fatal("stop expirado:", err)
	}
	if err := m.Start(ctx, "alpha", 0); err != nil {
		t.Fatal(err)
	}
	sb, _ = m.getSandbox(ctx, "alpha")
	if sb.Spec.ShutdownTime != nil {
		t.Fatal("deadline expirado preservado")
	}
}

func TestLegacyMigrationPreservesPodDataAndRejectsDrift(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "changed"}[drift], func(t *testing.T) {
			m := newTestManager()
			ctx := context.Background()
			mustCreate(t, m, "alpha")
			sb, svc, pvc, _ := m.desired("alpha")
			disabled := false
			sb.Spec.Service = &disabled
			fingerprint, _ := specFingerprint(sb.Spec, svc.Spec, pvc.Spec)
			sb.Annotations[SpecAnnotation] = fingerprint
			sb.UID = "sandbox-uid"
			if drift {
				sb.Spec.PodTemplate.Spec.Containers[0].Image = "arbitrary-image"
			}
			client := m.Sandboxes.(*sandboxfake.Clientset)
			if err := client.Tracker().Update(sandboxv1.GroupVersion.WithResource("sandboxes"), sb, Namespace); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Core.CoreV1().Services(Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			password, _ := m.Password(ctx, "alpha")
			err := m.Start(ctx, "alpha", 0)
			if drift {
				if err == nil || !strings.Contains(err.Error(), "migração recusada") {
					t.Fatal("drift aceito", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			sb, _ = m.getSandbox(ctx, "alpha")
			if sb.Spec.Service == nil || !*sb.Spec.Service {
				t.Fatal("Service não habilitado")
			}
			if _, err := m.Core.CoreV1().Services(Namespace).Get(ctx, svc.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatal("legado não removido", err)
			}
			if got, _ := m.Password(ctx, "alpha"); got != password {
				t.Fatal("senha alterada")
			}
		})
	}
}

func TestAutomaticServiceCollisionPreventsEveryMutation(t *testing.T) {
	for _, operation := range []string{"create", "start", "stop", "delete"} {
		t.Run(operation, func(t *testing.T) {
			m := newTestManager()
			ctx := context.Background()
			mustCreate(t, m, "alpha")
			svc, _ := m.Core.CoreV1().Services(Namespace).Get(ctx, base("alpha"), metav1.GetOptions{})
			svc.OwnerReferences[0].UID = "foreign"
			if _, err := m.Core.CoreV1().Services(Namespace).Update(ctx, svc, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			core := m.Core.(*corefake.Clientset)
			sbs := m.Sandboxes.(*sandboxfake.Clientset)
			core.ClearActions()
			sbs.ClearActions()
			var err error
			switch operation {
			case "create":
				err = m.Create(ctx, "alpha")
			case "start":
				err = m.Start(ctx, "alpha", 0)
			case "stop":
				err = m.Stop(ctx, "alpha")
			case "delete":
				err = m.Delete(ctx, "alpha", true)
			}
			if err == nil {
				t.Fatal("colisão aceita")
			}
			for _, a := range append(core.Actions(), sbs.Actions()...) {
				if a.GetVerb() != "get" {
					t.Fatal("mutação antes da colisão", a.GetVerb())
				}
			}
		})
	}
}

func TestListRejectsStaleReadyAndReportsTerminalFailure(t *testing.T) {
	m := newTestManager()
	ctx := context.Background()
	mustCreate(t, m, "alpha")
	sb, _ := m.getSandbox(ctx, "alpha")
	sb.Generation = 2
	sb.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 1}}
	client := m.Sandboxes.(*sandboxfake.Clientset)
	if err := client.Tracker().Update(sandboxv1.GroupVersion.WithResource("sandboxes"), sb, Namespace); err != nil {
		t.Fatal(err)
	}
	entries, _ := m.List(ctx)
	if entries[0].Status != "Pending" {
		t.Fatal(entries)
	}
	sb.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, ObservedGeneration: 2, Reason: sandboxv1.SandboxReasonReconcilerError}}
	if err := client.Tracker().Update(sandboxv1.GroupVersion.WithResource("sandboxes"), sb, Namespace); err != nil {
		t.Fatal(err)
	}
	entries, _ = m.List(ctx)
	if entries[0].Status != "Failed" {
		t.Fatal(entries)
	}
}

func TestCreateReportsExpirationDuringInitializationAndPreservesRetry(t *testing.T) {
	m := newTestManager()
	ctx := context.Background()
	err := m.CreateWithTTL(ctx, "alpha", time.Nanosecond)
	if err == nil || !strings.Contains(err.Error(), "expirou") {
		t.Fatalf("criação recém-expirada deve informar expiração, recebido %v", err)
	}
	sb, err := m.getSandbox(ctx, "alpha")
	if err != nil {
		t.Fatal("Sandbox parcial perdido:", err)
	}
	deadline := sb.Spec.ShutdownTime.DeepCopy()
	password, err := m.Password(ctx, "alpha")
	if err != nil {
		t.Fatal("Secret parcial perdido:", err)
	}
	if _, err := m.Core.CoreV1().PersistentVolumeClaims(Namespace).Get(ctx, base("alpha")+"-home", metav1.GetOptions{}); err != nil {
		t.Fatal("PVC parcial perdido:", err)
	}
	if err := m.CreateWithTTL(ctx, "alpha", time.Hour); err != nil {
		t.Fatal("repetição deve preservar ambiente previamente expirado:", err)
	}
	sb, _ = m.getSandbox(ctx, "alpha")
	if !sb.Spec.ShutdownTime.Equal(deadline) {
		t.Fatal("repetição renovou prazo expirado")
	}
	if got, _ := m.Password(ctx, "alpha"); got != password {
		t.Fatal("repetição alterou senha")
	}
}
