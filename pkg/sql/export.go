// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/cloud"
	"github.com/cockroachdb/cockroach/pkg/featureflag"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/exec"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/humanizeutil"
	"github.com/cockroachdb/errors"
)

type exportNode struct {
	optColumnsSlot

	source planNode

	// destination represents the destination URI for the export,
	// typically a directory
	destination string
	// fileNamePattern represents the file naming pattern for the
	// export, typically to be appended to the destination URI
	fileNamePattern string
	format          roachpb.IOFileFormat
	chunkRows       int
	chunkSize       int64
	colNames        []string
}

func (e *exportNode) startExec(params runParams) error {
	panic("exportNode cannot be run in local mode")
}

func (e *exportNode) Next(params runParams) (bool, error) {
	panic("exportNode cannot be run in local mode")
}

func (e *exportNode) Values() tree.Datums {
	panic("exportNode cannot be run in local mode")
}

func (e *exportNode) Close(ctx context.Context) {
	e.source.Close(ctx)
}

const (
	exportOptionDelimiter   = "delimiter"
	exportOptionNullAs      = "nullas"
	exportOptionChunkRows   = "chunk_rows"
	exportOptionChunkSize   = "chunk_size"
	exportOptionFileName    = "filename"
	exportOptionCompression = "compression"

	exportChunkSizeDefault = int64(32 << 20) // 32 MB
	exportChunkRowsDefault = 100000

	exportFilePatternPart = "%part%"
	exportGzipCodec       = "gzip"
	exportSnappyCodec     = "snappy"
	csvSuffix             = "csv"
	parquetSuffix         = "parquet"
)

var exportOptionExpectValues = map[string]KVStringOptValidate{
	exportOptionChunkRows:   KVStringOptRequireValue,
	exportOptionDelimiter:   KVStringOptRequireValue,
	exportOptionFileName:    KVStringOptRequireValue,
	exportOptionNullAs:      KVStringOptRequireValue,
	exportOptionCompression: KVStringOptRequireValue,
	exportOptionChunkSize:   KVStringOptRequireValue,
}

// featureExportEnabled is used to enable and disable the EXPORT feature.
var featureExportEnabled = settings.RegisterBoolSetting(
	settings.TenantWritable,
	"feature.export.enabled",
	"set to true to enable exports, false to disable; default is true",
	featureflag.FeatureFlagEnabledDefault,
).WithPublic()

// ConstructExport is part of the exec.Factory interface.
func (ef *execFactory) ConstructExport(
	input exec.Node,
	fileName tree.TypedExpr,
	fileSuffix string,
	options []exec.KVOption,
	notNullCols exec.NodeColumnOrdinalSet,
) (exec.Node, error) {
	fileSuffix = strings.ToLower(fileSuffix)

	if !featureExportEnabled.Get(&ef.planner.ExecCfg().Settings.SV) {
		return nil, pgerror.Newf(
			pgcode.OperatorIntervention,
			"feature EXPORT was disabled by the database administrator",
		)
	}

	if err := featureflag.CheckEnabled(
		ef.planner.EvalContext().Context,
		ef.planner.execCfg,
		featureExportEnabled,
		"EXPORT",
	); err != nil {
		return nil, err
	}

	if !ef.planner.IsAutoCommit() {
		return nil, errors.Errorf("EXPORT cannot be used inside a multi-statement transaction")
	}

	if fileSuffix != csvSuffix && fileSuffix != parquetSuffix {
		return nil, errors.Errorf("unsupported export format: %q", fileSuffix)
	}

	destinationDatum, err := fileName.Eval(ef.planner.EvalContext())
	if err != nil {
		return nil, err
	}

	destination, ok := destinationDatum.(*tree.DString)
	if !ok {
		return nil, errors.Errorf("expected string value for the file location")
	}
	admin, err := ef.planner.HasAdminRole(ef.planner.EvalContext().Context)
	if err != nil {
		panic(err)
	}
	if !admin && !ef.planner.ExecCfg().ExternalIODirConfig.EnableNonAdminImplicitAndArbitraryOutbound {
		conf, err := cloud.ExternalStorageConfFromURI(string(*destination), ef.planner.User())
		if err != nil {
			return nil, err
		}
		if !conf.AccessIsWithExplicitAuth() {
			panic(pgerror.Newf(
				pgcode.InsufficientPrivilege,
				"only users with the admin role are allowed to EXPORT to the specified URI"))
		}
	}
	optVals, err := evalStringOptions(ef.planner.EvalContext(), options, exportOptionExpectValues)
	if err != nil {
		return nil, err
	}

	cols := planColumns(input.(planNode))
	colNames := make([]string, len(cols))
	colNullability := make([]bool, len(cols))
	for i, col := range cols {
		colNames[i] = col.Name
		colNullability[i] = !notNullCols.Contains(i)
	}

	format := roachpb.IOFileFormat{}
	switch fileSuffix {
	case csvSuffix:
		csvOpts := roachpb.CSVOptions{}
		if override, ok := optVals[exportOptionDelimiter]; ok {
			csvOpts.Comma, err = util.GetSingleRune(override)
			if err != nil {
				return nil, pgerror.New(pgcode.InvalidParameterValue, "invalid delimiter")
			}
		}
		if override, ok := optVals[exportOptionNullAs]; ok {
			csvOpts.NullEncoding = &override
		}
		format.Format = roachpb.IOFileFormat_CSV
		format.Csv = csvOpts
	case parquetSuffix:
		parquetOpts := roachpb.ParquetOptions{
			ColNullability: colNullability,
		}
		format.Format = roachpb.IOFileFormat_Parquet
		format.Parquet = parquetOpts
	}

	chunkRows := exportChunkRowsDefault
	if override, ok := optVals[exportOptionChunkRows]; ok {
		chunkRows, err = strconv.Atoi(override)
		if err != nil {
			return nil, pgerror.WithCandidateCode(err, pgcode.InvalidParameterValue)
		}
		if chunkRows < 1 {
			return nil, pgerror.New(pgcode.InvalidParameterValue, "invalid csv chunk rows")
		}
	}

	chunkSize := exportChunkSizeDefault
	if override, ok := optVals[exportOptionChunkSize]; ok {
		chunkSize, err = humanizeutil.ParseBytes(override)
		if err != nil {
			return nil, pgerror.WithCandidateCode(err, pgcode.InvalidParameterValue)
		}
		if chunkSize < 1 {
			return nil, pgerror.New(pgcode.InvalidParameterValue, "invalid csv chunk size")
		}
	}

	// Check whenever compression is expected and extract compression codec name in case
	// of positive result
	var codec roachpb.IOFileFormat_Compression
	if name, ok := optVals[exportOptionCompression]; ok && len(name) != 0 {
		switch {
		case strings.EqualFold(name, exportGzipCodec):
			codec = roachpb.IOFileFormat_Gzip
		case strings.EqualFold(name, exportSnappyCodec) && fileSuffix == parquetSuffix:
			codec = roachpb.IOFileFormat_Snappy
		default:
			return nil, pgerror.Newf(pgcode.InvalidParameterValue,
				"unsupported compression codec %s for %s file format", name, fileSuffix)
		}
		format.Compression = codec
	}

	exportID := ef.planner.stmt.QueryID.String()
	exportFilePattern := exportFilePatternPart + "." + fileSuffix
	namePattern := fmt.Sprintf("export%s-%s", exportID, exportFilePattern)
	return &exportNode{
		source:          input.(planNode),
		destination:     string(*destination),
		fileNamePattern: namePattern,
		format:          format,
		chunkRows:       chunkRows,
		chunkSize:       chunkSize,
		colNames:        colNames,
	}, nil
}
