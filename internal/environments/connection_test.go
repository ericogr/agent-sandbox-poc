package environments

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestConnectGuardsClusterAndExplicitContext(t *testing.T) {
	for _, test := range []struct{ name, uid, version, wantError string }{{"matching", "recorded-uid", "v1.36.1", ""}, {"different-cluster", "foreign-uid", "v1.36.1", "UID do cluster diverge"}, {"unsupported", "recorded-uid", "v1.35.9", "cluster incompatível"}} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/namespaces/kube-system":
					fmt.Fprintf(w, `{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"kube-system","uid":%q}}`, test.uid)
				case "/api/v1/namespaces/default":
					fmt.Fprint(w, `{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"default"}}`)
				case "/version":
					fmt.Fprintf(w, `{"gitVersion":%q}`, test.version)
				default:
					t.Errorf("consulta inesperada: %s", r.URL.Path)
					http.Error(w, "unexpected", 500)
				}
			}))
			defer server.Close()
			root := t.TempDir()
			state := filepath.Join(root, ".state")
			if err := os.Mkdir(state, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, "cluster-uid"), []byte("recorded-uid\n"), 0600); err != nil {
				t.Fatal(err)
			}
			config := clientcmdapi.Config{CurrentContext: "kind-hub", Clusters: map[string]*clientcmdapi.Cluster{"poc": {Server: server.URL}, "hub": {Server: "http://127.0.0.1:1"}}, Contexts: map[string]*clientcmdapi.Context{Context: {Cluster: "poc"}, "kind-hub": {Cluster: "hub"}}}
			if err := clientcmd.WriteToFile(config, filepath.Join(state, "kubeconfig")); err != nil {
				t.Fatal(err)
			}
			_, _, err := Connect(context.Background(), root)
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("erro esperado %q, recebido %v", test.wantError, err)
			}
			if test.uid != "recorded-uid" && requests.Load() != 1 {
				t.Fatalf("consultas após UID divergente: %d", requests.Load())
			}
			unchanged, err := clientcmd.LoadFromFile(filepath.Join(state, "kubeconfig"))
			if err != nil || unchanged.CurrentContext != "kind-hub" {
				t.Fatal("contexto alterado")
			}
		})
	}
}
