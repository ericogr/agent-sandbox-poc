package manifests

import _ "embed"

//go:embed dev.yaml
var Dev []byte

//go:embed dev-storage.yaml
var Storage []byte
