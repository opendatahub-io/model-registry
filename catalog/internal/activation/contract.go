package activation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
)

const ProtocolVersion = "v1"

type Images struct {
	Catalog   string `json:"catalog"`
	Benchmark string `json:"benchmark"`
}

// Identity fences every outcome and serving authorization to one image pair.
type Identity struct {
	CatalogUID string `json:"catalogUID"`
	AttemptID  string `json:"attemptID"`
	Images     Images `json:"images"`
}

type Intent struct {
	Version     string `json:"version"`
	CatalogName string `json:"catalogName"`
	Generation  int64  `json:"generation"`
	RetryToken  string `json:"retryToken,omitempty"`
	Identity
	State          string    `json:"state"`
	Stage          string    `json:"stage"`
	Reason         string    `json:"reason"`
	Message        string    `json:"message"`
	LastSuccessful *Identity `json:"lastSuccessful,omitempty"`
}

type Report struct {
	Version string `json:"version"`
	Identity
	State       string `json:"state"`
	Stage       string `json:"stage"`
	Reason      string `json:"reason"`
	Message     string `json:"message"`
	ActivatedAt string `json:"activatedAt,omitempty"`
}

var immutableImage = regexp.MustCompile(`^.+@sha256:[a-f0-9]{64}$`)

func (id Identity) Validate() error {
	if id.CatalogUID == "" || id.AttemptID == "" || !immutableImage.MatchString(id.Images.Catalog) || !immutableImage.MatchString(id.Images.Benchmark) {
		return fmt.Errorf("activation requires Catalog UID, attempt ID and two immutable image references")
	}
	return nil
}

// SchemaName is derived solely from fenced identity, never SQL supplied by a
// caller. Attempts cannot mutate previous successful datasets.
func (id Identity) SchemaName() string {
	sum := sha256.Sum256([]byte(id.CatalogUID + "\x00" + id.AttemptID + "\x00" + id.Images.Catalog + "\x00" + id.Images.Benchmark))
	return "catalog_" + hex.EncodeToString(sum[:24])
}
