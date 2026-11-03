package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRejectsInvalidArgumentsBeforeClusterAccess(t *testing.T) {
	cases := [][]string{{"create"}, {"start"}, {"stop"}, {"create", "--ttl", "0s", "a"}, {"create", "--ttl", "-2s", "a"}, {"start", "--ttl", "nonsense", "a"}, {"start", "--ttl", "0", "a"}, {"stop", "--ttl", "1m", "a"}, {"create", "--parallel", "0", "a"}, {"create", "--parallel", "9", "a"}, {"create", "--timeout", "0s", "a"}, {"create", "a", "a"}, {"create", "../dev"}, {"open", "--port", "0", "a"}, {"open", "a", "b"}, {"password", "a", "b"}, {"list", "a"}, {"delete", "--purge", "go-dev/auth"}}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out bytes.Buffer
			err := run(context.Background(), args, &out, &out)
			if err == nil || strings.Contains(err.Error(), "propriedade do cluster") {
				t.Fatalf("não validou argumentos: %v", err)
			}
		})
	}
}
