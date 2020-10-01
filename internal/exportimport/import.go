// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package exportimport imports export files into the local database
package exportimport

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/google/exposure-notifications-server/internal/export"
	"github.com/google/exposure-notifications-server/internal/exportimport/model"
	exportproto "github.com/google/exposure-notifications-server/internal/pb/export"
	pubdb "github.com/google/exposure-notifications-server/internal/publish/database"
	pubmodel "github.com/google/exposure-notifications-server/internal/publish/model"
	"github.com/google/exposure-notifications-server/pkg/logging"
	"go.uber.org/zap"
)

type ImportRequest struct {
	config       *Config
	exportImport *model.ExportImport
	keys         []*model.ImportFilePublicKey
	file         *model.ImportFile
}

type ImportResposne struct {
	insertedKeys uint64
	revisedKeys  uint64
	droppedKeys  uint64
}

type SignatureAndKey struct {
	signature []byte
	publicKey *ecdsa.PublicKey
}

func (s *Server) ImportExportFile(ctx context.Context, ir *ImportRequest) (*ImportResposne, error) {
	logger := logging.FromContext(ctx)
	// Download zip file.
	client := &http.Client{
		Timeout: s.config.ExportFileDownloadTimeout,
	}
	resp, err := client.Get(ir.file.ZipFilename)
	if err != nil {
		return nil, fmt.Errorf("error downloading export file: %w", err)
	}

	defer resp.Body.Close()
	bytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading file: %w", err)
	}

	// Get bin and sig files.
	tekExport, digest, err := export.UnmarshalExportFile(bytes)
	if err != nil {
		return nil, fmt.Errorf("bin data error: %w", err)
	}
	tekSignatures, err := export.UnmarshalSignatureFile(bytes)
	if err != nil {
		return nil, fmt.Errorf("signature data missing: %w", err)
	}

	// Index the signatures from the file.
	signatures := make(map[string]*SignatureAndKey)
	for _, tekSig := range tekSignatures.GetSignatures() {
		idAndVersion := fmt.Sprintf("%s.%s", tekSig.SignatureInfo.GetVerificationKeyId(), tekSig.SignatureInfo.GetVerificationKeyVersion())
		signatures[idAndVersion] = &SignatureAndKey{
			signature: tekSig.GetSignature(),
		}
	}
	// Join in available public keys
	for _, key := range ir.keys {
		idAndVersion := fmt.Sprintf("%s.%s", key.KeyID, key.KeyVersion)
		if sak, ok := signatures[idAndVersion]; ok {
			sak.publicKey, err = key.PublicKey()
			if err != nil {
				return nil, fmt.Errorf("unable to parse public key: %w", err)
			}
		} else {
			logger.Infow("key not found...", "idAndVersion", idAndVersion)
		}
	}

	// Validate signatures.
	valid := false
	for k, sig := range signatures {
		if sig.publicKey == nil {
			logger.Warnw("no public key for signature", "signature", sig)
			continue
		}
		if ecdsa.VerifyASN1(sig.publicKey, digest[:], sig.signature) {
			valid = true
			logger.Debugw("validated signature", "file", ir.file, "kid.version", k)
			break
		}
	}
	if !valid {
		return nil, fmt.Errorf("no valid signature found")
	}

	// Common transform settings for primar + revised keys.
	exKeyTransform := transformer{
		appPackageName:               s.config.ImportAPKName,
		importRegions:                []string{ir.exportImport.Region},
		batchTime:                    time.Now().UTC().Truncate(s.config.CreatedAtTruncateWindow),
		exportImportID:               ir.exportImport.ID,
		importFileID:                 ir.file.ID,
		maxMagnitudeSymptomOnsetDays: int32(s.config.MaxMagnitudeSymptomOnsetDays),
		logger:                       logger,
	}
	response := ImportResposne{}

	// Go through primary keys and insert.
	// Must be separate from revised keys in the event both are in the same file.
	if len(tekExport.Keys) > 0 {
		inserts, dropped := exKeyTransform.transform(tekExport.Keys)
		response.droppedKeys = response.droppedKeys + dropped
		template := pubdb.InsertAndReviseExposuresRequest{
			SkipRevions: true,
		}
		if err := s.insertAndReviseKeys(ctx, "primary", inserts, &template, &response); err != nil {
			return nil, fmt.Errorf("insert primary keys: %w", err)
		}
	}

	// Then revised keys and revise.
	if len(tekExport.RevisedKeys) > 0 {
		revisions, dropped := exKeyTransform.transform(tekExport.RevisedKeys)
		response.droppedKeys = response.droppedKeys + dropped
		template := pubdb.InsertAndReviseExposuresRequest{
			OnlyRevisions:         true,
			RequireToken:          false,
			RequireExportImportID: true,
		}
		if err := s.insertAndReviseKeys(ctx, "revised", revisions, &template, &response); err != nil {
			return nil, fmt.Errorf("insert revised keys: %w", err)
		}
	}

	return &response, nil
}

func (s *Server) insertAndReviseKeys(ctx context.Context, mode string, exposures []*pubmodel.Exposure, template *pubdb.InsertAndReviseExposuresRequest, response *ImportResposne) error {
	logger := logging.FromContext(ctx)
	length := len(exposures)
	for i := 0; i < length; i = i + s.config.MaxInsertBatchSize {
		upper := i + s.config.MaxInsertBatchSize
		if upper > length {
			upper = length
		}
		// Assign the current operating slice.
		template.Incoming = exposures[i:upper]

		insertResponse, err := s.publishDB.InsertAndReviseExposures(ctx, template)
		if err != nil {
			return fmt.Errorf("publishDB.InsertAndReviseExposures: %w", err)
		}
		logger.Infow("insertAndRevise", "mode", mode, "candidates", len(template.Incoming), "inserted", insertResponse.Inserted, "revised", insertResponse.Revised, "dropped", insertResponse.Dropped)

		response.insertedKeys = response.insertedKeys + insertResponse.Inserted
		response.droppedKeys = response.droppedKeys + insertResponse.Dropped
		response.revisedKeys = response.revisedKeys + insertResponse.Revised
	}
	return nil
}

type transformer struct {
	appPackageName               string
	importRegions                []string
	batchTime                    time.Time
	exportImportID               int64
	importFileID                 int64
	maxMagnitudeSymptomOnsetDays int32
	logger                       *zap.SugaredLogger
}

func (t *transformer) transform(keys []*exportproto.TemporaryExposureKey) ([]*pubmodel.Exposure, uint64) {
	inserts := make([]*pubmodel.Exposure, 0, len(keys))
	var dropped uint64
	for _, k := range keys {
		exp, err := pubmodel.FromExportKey(k, t.maxMagnitudeSymptomOnsetDays)
		if err != nil {
			t.logger.Warnw("skipping invalid key", "error", err)
			dropped++
			continue
		}
		// Fill in items that are specific to this import.
		exp.AppPackageName = t.appPackageName
		exp.Regions = t.importRegions
		exp.CreatedAt = t.batchTime
		exp.LocalProvenance = false
		exp.ExportImportID = &t.exportImportID
		exp.ImportFileID = &t.importFileID

		inserts = append(inserts, exp)
	}
	return inserts, dropped
}