// Generated from schemas/resources/asset-exchange-v1.schema.json using the
// arop-wire-model-v1 object, const discriminator, and closed-field rules.
package asset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"time"
)

var (
	runIDPattern     = regexp.MustCompile(`^run_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	assetIDPattern   = regexp.MustCompile(`^asset_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	grantIDPattern   = regexp.MustCompile(`^grant_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	digestPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	mediaTypePattern = regexp.MustCompile(`^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$`)
	namePattern      = regexp.MustCompile(`(?:^|[\\/])\.\.(?:[\\/]|$)`)
	tokenPattern     = regexp.MustCompile(`^agt_[A-Za-z0-9_-]{8,128}$`)
	brokerPattern    = regexp.MustCompile(`^/v1/asset-content/grant_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	dateTimePattern  = regexp.MustCompile(`^[0-9]{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12][0-9]|3[01])T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:\.[0-9]+)?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$`)
)

type AssetRef struct {
	AssetID   string         `json:"asset_id"`
	Name      string         `json:"name"`
	MediaType string         `json:"media_type"`
	SizeBytes int64          `json:"size_bytes"`
	Digest    *string        `json:"digest,omitempty"`
	Access    AssetRefAccess `json:"access"`
}

type AssetRefAccess struct {
	Mode      string  `json:"mode"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

type UploadRequest struct {
	Kind      string  `json:"kind"`
	RunID     string  `json:"run_id"`
	Name      string  `json:"name"`
	MediaType string  `json:"media_type"`
	SizeBytes int64   `json:"size_bytes"`
	Digest    *string `json:"digest,omitempty"`
}

type DownloadRequest struct {
	Kind    string `json:"kind"`
	RunID   string `json:"run_id"`
	AssetID string `json:"asset_id"`
}

type GrantResponse struct {
	Kind        string   `json:"kind"`
	Asset       AssetRef `json:"asset"`
	GrantID     string   `json:"grant_id"`
	Method      string   `json:"method"`
	BrokerPath  string   `json:"broker_path"`
	BearerToken string   `json:"bearer_token"`
	ExpiresAt   string   `json:"expires_at"`
	MaxUses     int64    `json:"max_uses"`
}

type AssetExchange struct {
	UploadRequest   *UploadRequest
	DownloadRequest *DownloadRequest
	Grant           *GrantResponse
}

func DecodeAssetExchange(data []byte) (AssetExchange, error) {
	var value AssetExchange
	if err := value.UnmarshalJSON(data); err != nil {
		return AssetExchange{}, err
	}
	return value, nil
}

func (value AssetExchange) MarshalJSON() ([]byte, error) {
	switch {
	case value.UploadRequest != nil && value.DownloadRequest == nil && value.Grant == nil:
		return json.Marshal(value.UploadRequest)
	case value.DownloadRequest != nil && value.UploadRequest == nil && value.Grant == nil:
		return json.Marshal(value.DownloadRequest)
	case value.Grant != nil && value.UploadRequest == nil && value.DownloadRequest == nil:
		return json.Marshal(value.Grant)
	default:
		return nil, fmt.Errorf("asset exchange requires exactly one variant")
	}
}

func (value *AssetExchange) UnmarshalJSON(data []byte) error {
	var discriminator struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return err
	}
	*value = AssetExchange{}
	switch discriminator.Kind {
	case "upload_request":
		var candidate UploadRequest
		if err := decodeExact(data, &candidate); err != nil {
			return err
		}
		if err := candidate.validate(); err != nil {
			return err
		}
		value.UploadRequest = &candidate
	case "download_request":
		var candidate DownloadRequest
		if err := decodeExact(data, &candidate); err != nil {
			return err
		}
		if err := candidate.validate(); err != nil {
			return err
		}
		value.DownloadRequest = &candidate
	case "grant":
		var candidate GrantResponse
		if err := decodeExact(data, &candidate); err != nil {
			return err
		}
		if err := candidate.validate(); err != nil {
			return err
		}
		value.Grant = &candidate
	default:
		return fmt.Errorf("unknown asset exchange discriminator %q", discriminator.Kind)
	}
	return nil
}

func (value UploadRequest) validate() error {
	if value.Kind != "upload_request" {
		return fmt.Errorf("unexpected upload request kind")
	}
	if !runIDPattern.MatchString(value.RunID) || !validName(value.Name) || !mediaTypePattern.MatchString(value.MediaType) || value.SizeBytes < 0 {
		return fmt.Errorf("invalid upload request")
	}
	if value.Digest != nil && !digestPattern.MatchString(*value.Digest) {
		return fmt.Errorf("invalid upload digest")
	}
	return nil
}

func (value DownloadRequest) validate() error {
	if value.Kind != "download_request" || !runIDPattern.MatchString(value.RunID) || !assetIDPattern.MatchString(value.AssetID) {
		return fmt.Errorf("invalid download request")
	}
	return nil
}

func (value GrantResponse) validate() error {
	if value.Kind != "grant" || (value.Method != "upload" && value.Method != "download") {
		return fmt.Errorf("invalid grant")
	}
	if !grantIDPattern.MatchString(value.GrantID) || !brokerPattern.MatchString(value.BrokerPath) || !tokenPattern.MatchString(value.BearerToken) || value.MaxUses < 1 {
		return fmt.Errorf("invalid grant limits")
	}
	if !validDateTime(value.ExpiresAt) || value.Asset.validate() != nil {
		return fmt.Errorf("invalid grant asset")
	}
	return nil
}

func (value AssetRef) validate() error {
	if !assetIDPattern.MatchString(value.AssetID) || !validName(value.Name) || !mediaTypePattern.MatchString(value.MediaType) || value.SizeBytes < 0 || value.Access.Mode != "brokered" {
		return fmt.Errorf("invalid asset ref")
	}
	if value.Digest != nil && !digestPattern.MatchString(*value.Digest) {
		return fmt.Errorf("invalid asset digest")
	}
	if value.Access.ExpiresAt != nil && !validDateTime(*value.Access.ExpiresAt) {
		return fmt.Errorf("invalid asset expiry")
	}
	return nil
}

func validName(value string) bool {
	return len(value) >= 1 && len(value) <= 512 && !namePattern.MatchString(value)
}

func validDateTime(value string) bool {
	if !dateTimePattern.MatchString(value) {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

func decodeExact(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}
