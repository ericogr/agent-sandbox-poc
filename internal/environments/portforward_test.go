package environments

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	httpstreamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func serveOpenFixture(t *testing.T, handshake http.HandlerFunc) (*Manager, *rest.Config) {
	t.Helper()
	m := newTestManager()
	mustCreate(t, m, "alpha")
	ctx := context.Background()
	responses := map[string]interface{}{}
	resources, err := m.resources("alpha")
	if err != nil {
		t.Fatal(err)
	}
	plural := map[string]string{"Secret": "secrets", "PVC": "persistentvolumeclaims", "Service": "services"}
	for _, resource := range resources {
		if resource.kind == "Sandbox" || resource.kind == "Service" {
			continue
		}
		object, err := resource.get(ctx)
		if err != nil {
			t.Fatal(err)
		}
		responses["/api/v1/namespaces/default/"+plural[resource.kind]+"/"+object.GetName()] = object
	}
	sandbox, err := m.Sandboxes.AgentsV1beta1().Sandboxes(Namespace).Get(ctx, base("alpha"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod, err := m.getPod(ctx, "alpha", sandbox)
	if err != nil {
		t.Fatal(err)
	}
	responses["/api/v1/namespaces/default/pods/"+pod.Name] = pod
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/portforward") {
			handshake(w, r)
			return
		}
		object, ok := responses[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(object)
	}))
	t.Cleanup(server.Close)
	config := &rest.Config{Host: server.URL}
	m.Core, err = kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return m, config
}

func TestOpenBoundsBlockedHandshake(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			m, config := serveOpenFixture(t, func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
				}
				http.Error(w, "closed", http.StatusServiceUnavailable)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timeout := 200 * time.Millisecond
			if mode == "cancel" {
				timeout = 5 * time.Second
			}
			done := make(chan error, 1)
			go func() { done <- m.Open(ctx, config, "alpha", 18099, timeout, io.Discard, io.Discard) }()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("não chegou ao handshake: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("handshake não iniciado")
			}
			want := context.DeadlineExceeded.Error()
			if mode == "cancel" {
				cancel()
				want = context.Canceled.Error()
			}
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("erro esperado %s, recebido %v", want, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("handshake ignorou timeout/cancelamento")
			}
		})
	}
}

func TestEstablishedSPDYConnectionSurvivesOpeningDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := httpstream.Handshake(r, w, []string{"portforward.k8s.io"}); err != nil {
			return
		}
		connection := httpstreamspdy.NewResponseUpgrader().UpgradeResponse(w, r, func(stream httpstream.Stream, replySent <-chan struct{}) error {
			go func() { defer stream.Close(); <-replySent; _, _ = io.Copy(stream, stream) }()
			return nil
		})
		if connection != nil {
			defer connection.Close()
			<-connection.CloseChan()
		}
	}))
	defer server.Close()
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	connection, _, err := (openingDialer{ctx: ctx, config: &rest.Config{Host: server.URL}, url: address}).Dial("portforward.k8s.io")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	<-ctx.Done()
	select {
	case <-connection.CloseChan():
		t.Fatal("deadline de abertura encerrou sessão estabelecida")
	default:
	}
	done := make(chan error, 1)
	go func() {
		stream, err := connection.CreateStream(http.Header{})
		if err != nil {
			done <- err
			return
		}
		defer stream.Close()
		if _, err = stream.Write([]byte("healthy")); err != nil {
			done <- err
			return
		}
		data := make([]byte, 7)
		_, err = io.ReadFull(stream, data)
		if err == nil && string(data) != "healthy" {
			err = fmt.Errorf("resposta inesperada: %q", data)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("conexão após deadline:", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("conexão não respondeu após deadline de abertura")
	}
}
