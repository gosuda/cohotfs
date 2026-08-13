package containeragent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const BootstrapAPI = "v1alpha2"

type BaseMarker struct {
	APIVersion   string `json:"apiVersion"`
	Kind         string `json:"kind"`
	BootstrapAPI string `json:"bootstrapAPI"`
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	AgentPath    string `json:"agentPath"`
}

func CheckImage(root, requestedAPI string) (BaseMarker, error) {
	if root == "" {
		root = "/"
	}
	imageRoot, err := os.OpenRoot(root)
	if err != nil {
		return BaseMarker{}, fmt.Errorf("image_incompatible: open image root: %w", err)
	}
	defer imageRoot.Close()
	file, err := imageRoot.Open(".cohotfs/base.json")
	if err != nil {
		return BaseMarker{}, fmt.Errorf("image_incompatible: open base marker: %w", err)
	}
	defer file.Close()
	var marker BaseMarker
	if err := decodeOneJSON(file, &marker); err != nil {
		return BaseMarker{}, fmt.Errorf("image_incompatible: parse base marker: %w", err)
	}
	if marker.APIVersion != "cohotfs.io/v1alpha1" || marker.Kind != "CohotfsBase" || marker.BootstrapAPI != requestedAPI || marker.OS != "linux" || marker.Architecture != "amd64" || marker.AgentPath != "/usr/local/libexec/cohotfs-agent" {
		return BaseMarker{}, fmt.Errorf("image_incompatible: base marker contract mismatch")
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return BaseMarker{}, fmt.Errorf("image_incompatible: agent platform %s/%s is unsupported", runtime.GOOS, runtime.GOARCH)
	}
	for _, required := range []struct {
		path         string
		allowSymlink bool
	}{{marker.AgentPath, false}, {"/usr/sbin/sshd", true}, {"/bin/sh", true}, {"/bin/bash", true}} {
		path := strings.TrimPrefix(required.path, "/")
		info, err := imageRoot.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 && !required.allowSymlink {
			return BaseMarker{}, fmt.Errorf("image_incompatible: required executable %s is unavailable", required.path)
		}
		info, err = imageRoot.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return BaseMarker{}, fmt.Errorf("image_incompatible: required executable %s is unavailable", required.path)
		}
	}
	return marker, nil
}

func rooted(root, absolute string) string {
	if root == "" || root == "/" {
		return absolute
	}
	return filepath.Join(root, strings.TrimPrefix(absolute, "/"))
}

func decodeOneJSON(reader io.Reader, value any) error {
	raw, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return fmt.Errorf("JSON document exceeds 1 MiB")
	}
	if err := rejectDuplicateJSONFields(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("expected one JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateJSONFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("expected one JSON value")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		fields := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object key is not a string")
			}
			if _, duplicate := fields[key]; duplicate {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			fields[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("invalid JSON array")
		}
	default:
		return fmt.Errorf("invalid JSON delimiter %q", delimiter)
	}
	return nil
}
