// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package metadata

// metadata package implements writing and reading wrapped meta.json where Thanos puts its metadata.
// Those metadata contains external labels, downsampling resolution and source type.
// This package is minimal and separated because it used by testutils which limits test helpers we can use in
// this package.

import (
	"encoding/json"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/go-kit/kit/log"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/fileutil"
	"github.com/thanos-io/thanos/pkg/runutil"
)

type SourceType string

const (
	UnknownSource         SourceType = ""
	SidecarSource         SourceType = "sidecar"
	ReceiveSource         SourceType = "receive"
	CompactorSource       SourceType = "compactor"
	CompactorRepairSource SourceType = "compactor.repair"
	RulerSource           SourceType = "ruler"
	BucketRepairSource    SourceType = "bucket.repair"
	TestSource            SourceType = "test"
)

const (
	// MetaFilename is the known JSON filename for meta information.
	MetaFilename = "meta.json"
	// TSDBVersion1 is a enumeration of TSDB meta versions supported by Thanos.
	TSDBVersion1 = 1
	// ThanosVersion1 is a enumeration of Thanos section of TSDB meta supported by Thanos.
	ThanosVersion1 = 1
)

// Meta describes the a block's meta. It wraps the known TSDB meta structure and
// extends it by Thanos-specific fields.
type Meta struct {
	tsdb.BlockMeta

	Thanos Thanos `json:"thanos"`
}

// Thanos holds block meta information specific to Thanos.
type Thanos struct {
	// Version of Thanos meta file. If none specified, 1 is assumed (since first version did not have explicit version specified).
	Version int `json:"version,omitempty"`

	Labels     map[string]string `json:"labels"`
	Downsample ThanosDownsample  `json:"downsample"`

	// Source is a real upload source of the block.
	Source SourceType `json:"source"`

	// List of segment files (in chunks directory), in sorted order. Optional.
	// Deprecated. Use Files instead.
	SegmentFiles []string `json:"segment_files,omitempty"`

	// File is a sorted (by rel path) list of all files in block directory of this block known to TSDB.
	// Sorted by relative path.
	// Useful to avoid API call to get size of each file, as well as for debugging purposes.
	// Optional, added in v0.17.0.
	Files []File `json:"files,omitempty"`
}

type File struct {
	RelPath string `json:"rel_path"`
	// SizeBytes is optional (e.g meta.json does not show size).
	SizeBytes int64 `json:"size_bytes,omitempty"`
}

type ThanosDownsample struct {
	Resolution int64 `json:"resolution"`
}

// InjectThanos sets Thanos meta to the block meta JSON and saves it to the disk.
// NOTE: It should be used after writing any block by any Thanos component, otherwise we will miss crucial metadata.
func InjectThanos(logger log.Logger, bdir string, meta Thanos, downsampledMeta *tsdb.BlockMeta) (*Meta, error) {
	newMeta, err := Read(bdir)
	if err != nil {
		return nil, errors.Wrap(err, "read new meta")
	}
	newMeta.Thanos = meta

	// While downsampling we need to copy original compaction.
	if downsampledMeta != nil {
		newMeta.Compaction = downsampledMeta.Compaction
	}

	if err := newMeta.WriteToDir(logger, bdir); err != nil {
		return nil, errors.Wrap(err, "write new meta")
	}

	return newMeta, nil
}

// WriteToDir writes the encoded meta into <dir>/meta.json.
func (m Meta) WriteToDir(logger log.Logger, dir string) error {
	// Make any changes to the file appear atomic.
	path := filepath.Join(dir, MetaFilename)
	tmp := path + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	if err := m.Write(f); err != nil {
		runutil.CloseWithLogOnErr(logger, f, "close meta")
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return renameFile(logger, tmp, path)
}

// Write writes the given encoded meta to writer.
func (m Meta) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "\t")
	return enc.Encode(&m)
}

func renameFile(logger log.Logger, from, to string) error {
	if err := os.RemoveAll(to); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return err
	}

	// Directory was renamed; sync parent dir to persist rename.
	pdir, err := fileutil.OpenDir(filepath.Dir(to))
	if err != nil {
		return err
	}

	if err = fileutil.Fdatasync(pdir); err != nil {
		runutil.CloseWithLogOnErr(logger, pdir, "close dir")
		return err
	}
	return pdir.Close()
}

// Read reads the given meta from <dir>/meta.json.
func Read(dir string) (*Meta, error) {
	b, err := ioutil.ReadFile(filepath.Join(dir, MetaFilename))
	if err != nil {
		return nil, err
	}
	var m Meta

	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Version != TSDBVersion1 {
		return nil, errors.Errorf("unexpected meta file version %d", m.Version)
	}

	version := m.Thanos.Version
	if version == 0 {
		// For compatibility.
		version = ThanosVersion1
	}

	if version != ThanosVersion1 {
		return nil, errors.Errorf("unexpected meta file Thanos section version %d", m.Version)
	}
	return &m, nil
}
