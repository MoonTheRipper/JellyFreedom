package store

import (
	"encoding/json"
	"strings"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
func containsStr(h, n string) bool      { return strings.Contains(h, n) }
