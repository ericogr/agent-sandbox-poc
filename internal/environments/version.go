package environments

import (
	"encoding/json"
	"strings"
)

func supportedVersion(data []byte) bool {
	var version struct {
		GitVersion string `json:"gitVersion"`
	}
	return json.Unmarshal(data, &version) == nil && strings.HasPrefix(version.GitVersion, "v1.36.")
}
