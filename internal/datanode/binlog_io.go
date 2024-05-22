// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package datanode

import (
	"context"
	"path"
	"strconv"
	"time"

	"github.com/cockroachdb/errors"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/datanode/allocator"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/etcdpb"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/pkg/common"
	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/util/conc"
	"github.com/milvus-io/milvus/pkg/util/merr"
	"github.com/milvus-io/milvus/pkg/util/metautil"
	"github.com/milvus-io/milvus/pkg/util/retry"
)

var errUploadToBlobStorage = errors.New("upload to blob storage wrong")

type downloader interface {
	// donload downloads insert-binlogs, stats-binlogs, and, delta-binlogs from blob storage for given paths.
	// The paths are 1 group of binlog paths generated by 1 `Serialize`.
	//
	// throw errors promptly instead of needlessly retrying forever after v2.3.16
	download(ctx context.Context, paths []string) ([]*Blob, error)
}

type uploader interface {
	// upload saves InsertData and DeleteData into blob storage, stats binlogs are generated from InsertData.
	//
	// errUploadToBlobStorage is returned if ctx is canceled from outside while a uploading is inprogress.
	// Beware of the ctx here, if no timeout or cancel is applied to this ctx, this uploading may retry forever.
	uploadInsertLog(ctx context.Context, segID, partID UniqueID, iData *InsertData, meta *etcdpb.CollectionMeta) (map[UniqueID]*datapb.FieldBinlog, error)
	uploadStatsLog(ctx context.Context, segID, partID UniqueID, iData *InsertData, stats *storage.PrimaryKeyStats, totRows int64, meta *etcdpb.CollectionMeta) (map[UniqueID]*datapb.FieldBinlog, map[UniqueID]*datapb.FieldBinlog, error)
	uploadDeltaLog(ctx context.Context, segID, partID UniqueID, dData *DeleteData, meta *etcdpb.CollectionMeta) ([]*datapb.FieldBinlog, error)
}

type binlogIO struct {
	storage.ChunkManager
	allocator.Allocator
}

var (
	_ downloader = (*binlogIO)(nil)
	_ uploader   = (*binlogIO)(nil)
)

func (b *binlogIO) download(ctx context.Context, paths []string) ([]*Blob, error) {
	log.Debug("down load", zap.Strings("path", paths))
	resp := make([]*Blob, len(paths))
	if len(paths) == 0 {
		return resp, nil
	}
	futures := make([]*conc.Future[any], len(paths))
	for i, path := range paths {
		localPath := path
		future := getMultiReadPool().Submit(func() (any, error) {
			var val []byte
			var err error

			log.Debug("binlogIO download", zap.String("path", localPath))
			err = retry.Do(ctx, func() error {
				val, err = b.Read(ctx, localPath)
				if err != nil {
					log.Warn("binlogIO fail to download", zap.String("path", localPath), zap.Error(err))
				}
				return err
			}, retry.Attempts(3), retry.RetryErr(merr.IsRetryableErr))

			return val, err
		})
		futures[i] = future
	}

	for i := range futures {
		if !futures[i].OK() {
			return nil, futures[i].Err()
		}
		resp[i] = &Blob{Value: futures[i].Value().([]byte)}
	}

	return resp, nil
}

func (b *binlogIO) uploadSegmentFiles(
	ctx context.Context,
	CollectionID UniqueID,
	segID UniqueID,
	kvs map[string][]byte,
) error {
	log.Debug("update", zap.Int64("collectionID", CollectionID), zap.Int64("segmentID", segID))
	if len(kvs) == 0 {
		return nil
	}
	futures := make([]*conc.Future[any], 0)
	for key, val := range kvs {
		localPath := key
		localVal := val
		future := getMultiReadPool().Submit(func() (any, error) {
			err := errStart
			for err != nil {
				select {
				case <-ctx.Done():
					log.Warn("ctx done when saving kvs to blob storage",
						zap.Int64("collectionID", CollectionID),
						zap.Int64("segmentID", segID),
						zap.Int("number of kvs", len(kvs)))
					return nil, errUploadToBlobStorage
				default:
					if err != errStart {
						time.Sleep(50 * time.Millisecond)
					}
					err = b.Write(ctx, localPath, localVal)
				}
			}
			return nil, nil
		})
		futures = append(futures, future)
	}

	err := conc.AwaitAll(futures...)
	if err != nil {
		return err
	}
	return nil
}

// genDeltaBlobs returns key, value
func (b *binlogIO) genDeltaBlobs(data *DeleteData, collID, partID, segID UniqueID) (string, []byte, error) {
	dCodec := storage.NewDeleteCodec()

	blob, err := dCodec.Serialize(collID, partID, segID, data)
	if err != nil {
		return "", nil, err
	}

	idx, err := b.AllocOne()
	if err != nil {
		return "", nil, err
	}
	k := metautil.JoinIDPath(collID, partID, segID, idx)

	key := path.Join(b.ChunkManager.RootPath(), common.SegmentDeltaLogPath, k)

	return key, blob.GetValue(), nil
}

// genInsertBlobs returns insert-paths and save blob to kvs
func (b *binlogIO) genInsertBlobs(data *InsertData, partID, segID UniqueID, iCodec *storage.InsertCodec, kvs map[string][]byte) (map[UniqueID]*datapb.FieldBinlog, error) {
	inlogs, err := iCodec.Serialize(partID, segID, data)
	if err != nil {
		return nil, err
	}

	inpaths := make(map[UniqueID]*datapb.FieldBinlog)
	notifyGenIdx := make(chan struct{})
	defer close(notifyGenIdx)

	generator, err := b.GetGenerator(len(inlogs), notifyGenIdx)
	if err != nil {
		return nil, err
	}

	for _, blob := range inlogs {
		// Blob Key is generated by Serialize from int64 fieldID in collection schema, which won't raise error in ParseInt
		fID, _ := strconv.ParseInt(blob.GetKey(), 10, 64)
		k := metautil.JoinIDPath(iCodec.Schema.GetID(), partID, segID, fID, <-generator)
		key := path.Join(b.ChunkManager.RootPath(), common.SegmentInsertLogPath, k)

		value := blob.GetValue()
		fileLen := len(value)

		kvs[key] = value
		inpaths[fID] = &datapb.FieldBinlog{
			FieldID: fID,
			Binlogs: []*datapb.Binlog{{LogSize: int64(fileLen), LogPath: key, EntriesNum: blob.RowNum}},
		}
	}

	return inpaths, nil
}

// genStatBlobs return stats log paths and save blob to kvs
func (b *binlogIO) genStatBlobs(stats *storage.PrimaryKeyStats, partID, segID UniqueID, iCodec *storage.InsertCodec, kvs map[string][]byte, totRows int64) (map[UniqueID]*datapb.FieldBinlog, error) {
	statBlob, err := iCodec.SerializePkStats(stats, totRows)
	if err != nil {
		return nil, err
	}
	statPaths := make(map[UniqueID]*datapb.FieldBinlog)

	idx, err := b.AllocOne()
	if err != nil {
		return nil, err
	}

	fID, _ := strconv.ParseInt(statBlob.GetKey(), 10, 64)
	k := metautil.JoinIDPath(iCodec.Schema.GetID(), partID, segID, fID, idx)
	key := path.Join(b.ChunkManager.RootPath(), common.SegmentStatslogPath, k)

	value := statBlob.GetValue()
	fileLen := len(value)

	kvs[key] = value

	statPaths[fID] = &datapb.FieldBinlog{
		FieldID: fID,
		Binlogs: []*datapb.Binlog{{LogSize: int64(fileLen), LogPath: key, EntriesNum: totRows}},
	}
	return statPaths, nil
}

// update stats log
// also update with insert data if not nil
func (b *binlogIO) uploadStatsLog(
	ctx context.Context,
	segID UniqueID,
	partID UniqueID,
	iData *InsertData,
	stats *storage.PrimaryKeyStats,
	totRows int64,
	meta *etcdpb.CollectionMeta,
) (map[UniqueID]*datapb.FieldBinlog, map[UniqueID]*datapb.FieldBinlog, error) {
	var inPaths map[int64]*datapb.FieldBinlog
	var err error

	iCodec := storage.NewInsertCodecWithSchema(meta)
	kvs := make(map[string][]byte)

	if !iData.IsEmpty() {
		inPaths, err = b.genInsertBlobs(iData, partID, segID, iCodec, kvs)
		if err != nil {
			log.Warn("generate insert blobs wrong",
				zap.Int64("collectionID", iCodec.Schema.GetID()),
				zap.Int64("segmentID", segID),
				zap.Error(err))
			return nil, nil, err
		}
	}

	statPaths, err := b.genStatBlobs(stats, partID, segID, iCodec, kvs, totRows)
	if err != nil {
		return nil, nil, err
	}

	err = b.uploadSegmentFiles(ctx, meta.GetID(), segID, kvs)
	if err != nil {
		return nil, nil, err
	}

	return inPaths, statPaths, nil
}

func (b *binlogIO) uploadInsertLog(
	ctx context.Context,
	segID UniqueID,
	partID UniqueID,
	iData *InsertData,
	meta *etcdpb.CollectionMeta,
) (map[UniqueID]*datapb.FieldBinlog, error) {
	iCodec := storage.NewInsertCodecWithSchema(meta)
	kvs := make(map[string][]byte)

	if iData.IsEmpty() {
		log.Warn("binlog io uploading empty insert data",
			zap.Int64("segmentID", segID),
			zap.Int64("collectionID", iCodec.Schema.GetID()),
		)
		return nil, nil
	}

	inpaths, err := b.genInsertBlobs(iData, partID, segID, iCodec, kvs)
	if err != nil {
		return nil, err
	}

	err = b.uploadSegmentFiles(ctx, meta.GetID(), segID, kvs)
	if err != nil {
		return nil, err
	}

	return inpaths, nil
}

func (b *binlogIO) uploadDeltaLog(
	ctx context.Context,
	segID UniqueID,
	partID UniqueID,
	dData *DeleteData,
	meta *etcdpb.CollectionMeta,
) ([]*datapb.FieldBinlog, error) {
	var (
		deltaInfo = make([]*datapb.FieldBinlog, 0)
		kvs       = make(map[string][]byte)
	)

	if dData.RowCount > 0 {
		k, v, err := b.genDeltaBlobs(dData, meta.GetID(), partID, segID)
		if err != nil {
			log.Warn("generate delta blobs wrong",
				zap.Int64("collectionID", meta.GetID()),
				zap.Int64("segmentID", segID),
				zap.Error(err))
			return nil, err
		}

		kvs[k] = v
		deltaInfo = append(deltaInfo, &datapb.FieldBinlog{
			FieldID: 0, // TODO: Not useful on deltalogs, FieldID shall be ID of primary key field
			Binlogs: []*datapb.Binlog{{
				EntriesNum: dData.RowCount,
				LogPath:    k,
				LogSize:    int64(len(v)),
			}},
		})
	} else {
		return nil, nil
	}

	err := b.uploadSegmentFiles(ctx, meta.GetID(), segID, kvs)
	if err != nil {
		return nil, err
	}

	return deltaInfo, nil
}
