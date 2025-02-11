// Copyright 2022 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package allocator

import (
	"context"
	"encoding/json"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubefs/cubefs/blobstore/api/clustermgr"
	"github.com/cubefs/cubefs/blobstore/api/proxy"
	"github.com/cubefs/cubefs/blobstore/common/codemode"
	errcode "github.com/cubefs/cubefs/blobstore/common/errors"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/common/trace"
	"github.com/cubefs/cubefs/blobstore/util/defaulter"
	"github.com/cubefs/cubefs/blobstore/util/errors"
	"github.com/cubefs/cubefs/blobstore/util/retry"
)

const (
	defaultRetainIntervalS     = int64(40)
	defaultAllocVolsNum        = 1
	defaultTotalThresholdRatio = 0.6
	defaultInitVolumeNum       = 4
	defaultMetricIntervalS     = 60
)

type VolConfig struct {
	ClusterID             proto.ClusterID `json:"cluster_id"`
	Idc                   string          `json:"idc"`
	RetainIntervalS       int64           `json:"retain_interval_s"`
	DefaultAllocVolsNum   int             `json:"default_alloc_vols_num"`
	InitVolumeNum         int             `json:"init_volume_num"`
	TotalThresholdRatio   float64         `json:"total_threshold_ratio"`
	MetricReportIntervalS int             `json:"metric_report_interval_s"`
	VolumeReserveSize     int             `json:"-"`
}

type ModeInfo struct {
	volumes        *volumes
	totalThreshold uint64
	totalFree      uint64
}

type allocArgs struct {
	isInit   bool
	codeMode codemode.CodeMode
	count    int
}

type volumeMgr struct {
	BlobConfig
	VolConfig

	BidMgr

	clusterMgr clustermgr.APIProxy
	modeInfos  map[codemode.CodeMode]*ModeInfo
	mu         sync.RWMutex
	allocChs   map[codemode.CodeMode]chan *allocArgs
	preIdx     uint64
	closeCh    chan struct{}
}

func volConfCheck(cfg *VolConfig) {
	defaulter.Equal(&cfg.DefaultAllocVolsNum, defaultAllocVolsNum)
	defaulter.Equal(&cfg.RetainIntervalS, defaultRetainIntervalS)
	defaulter.Equal(&cfg.TotalThresholdRatio, defaultTotalThresholdRatio)
	defaulter.Equal(&cfg.InitVolumeNum, defaultInitVolumeNum)
	defaulter.Equal(&cfg.MetricReportIntervalS, defaultMetricIntervalS)
}

type VolumeMgr interface {
	// Alloc the required volumes to access module
	Alloc(ctx context.Context, args *proxy.AllocVolsArgs) (allocVols []proxy.AllocRet, err error)
	// List the volumes in the allocator
	List(ctx context.Context, codeMode codemode.CodeMode) (vids []proto.Vid, volumes []clustermgr.AllocVolumeInfo, err error)
	Close()
}

func (v *volumeMgr) Close() {
	span, _ := trace.StartSpanFromContextWithTraceID(context.Background(), "", "volumeMgrClose")
	close(v.closeCh)
	span.Warnf("close closeCh done")
}

func NewVolumeMgr(ctx context.Context, blobCfg BlobConfig, volCfg VolConfig, clusterMgr clustermgr.APIProxy) (VolumeMgr, error) {
	span := trace.SpanFromContextSafe(ctx)
	volConfCheck(&volCfg)
	bidMgr, err := NewBidMgr(ctx, blobCfg, clusterMgr)
	if err != nil {
		span.Fatalf("fail to new bidMgr, error:%v", err)
	}
	rand.Seed(int64(time.Now().Nanosecond()))
	v := &volumeMgr{
		clusterMgr: clusterMgr,
		modeInfos:  make(map[codemode.CodeMode]*ModeInfo),
		allocChs:   make(map[codemode.CodeMode]chan *allocArgs),
		BidMgr:     bidMgr,
		mu:         sync.RWMutex{},
		BlobConfig: blobCfg,
		VolConfig:  volCfg,
		closeCh:    make(chan struct{}),
	}
	atomic.StoreUint64(&v.preIdx, rand.Uint64())
	err = v.initModeInfo(ctx)
	if err != nil {
		return nil, err
	}

	go v.retainTask()
	go v.metricReportTask()

	return v, err
}

func (v *volumeMgr) initModeInfo(ctx context.Context) (err error) {
	span := trace.SpanFromContextSafe(ctx)
	volumeReserveSize, err := v.clusterMgr.GetConfig(ctx, proto.VolumeReserveSizeKey)
	if err != nil {
		return errors.Base(err, "Get volume_reserve_size config from clusterMgr err").Detail(err)
	}
	v.VolConfig.VolumeReserveSize, err = strconv.Atoi(volumeReserveSize)
	if err != nil {
		return errors.Base(err, "strconv.Atoi volumeReserveSize err").Detail(err)
	}
	volumeChunkSize, err := v.clusterMgr.GetConfig(ctx, proto.VolumeChunkSizeKey)
	if err != nil {
		return errors.Base(err, "Get volume_chunk_size config from clusterMgr err").Detail(err)
	}
	volumeChunkSizeInt, err := strconv.Atoi(volumeChunkSize)
	if err != nil {
		return errors.Base(err, "strconv.Atoi volumeChunkSize err").Detail(err)
	}
	codeModeInfos, err := v.clusterMgr.GetConfig(ctx, proto.CodeModeConfigKey)
	if err != nil {
		return errors.Base(err, "Get code_mode config from clusterMgr err").Detail(err)
	}
	codeModeConfigInfos := make([]codemode.Policy, 0)
	err = json.Unmarshal([]byte(codeModeInfos), &codeModeConfigInfos)
	if err != nil {
		return errors.Base(err, "json.Unmarshal code_mode policy err").Detail(err)
	}
	for _, codeModeConfig := range codeModeConfigInfos {
		allocCh := make(chan *allocArgs)
		codeMode := codeModeConfig.ModeName.GetCodeMode()
		if !codeModeConfig.Enable {
			continue
		}
		v.allocChs[codeMode] = allocCh
		tactic := codeMode.Tactic()
		threshold := float64(v.InitVolumeNum*tactic.N*volumeChunkSizeInt) * v.TotalThresholdRatio
		modeInfo := &ModeInfo{
			volumes:        &volumes{},
			totalThreshold: uint64(threshold),
		}
		v.modeInfos[codeMode] = modeInfo
		span.Infof("code_mode: %v, initVolumeNum: %v, threshold: %v", codeModeConfig.ModeName, v.InitVolumeNum, threshold)
	}

	for mode := range v.allocChs {
		applyArg := &allocArgs{
			isInit:   true,
			codeMode: mode,
			count:    v.InitVolumeNum,
		}

		go v.allocVolumeLoop(mode)
		v.allocChs[mode] <- applyArg

	}
	return
}

func (v *volumeMgr) Alloc(ctx context.Context, args *proxy.AllocVolsArgs) (allocRets []proxy.AllocRet, err error) {
	allocBidScopes, err := v.BidMgr.Alloc(ctx, args.BidCount)
	if err != nil {
		return nil, err
	}
	vid, err := v.allocVid(ctx, args)
	if err != nil {
		return nil, err
	}
	allocRets = make([]proxy.AllocRet, 0, 128)
	for _, bidScope := range allocBidScopes {
		volRet := proxy.AllocRet{
			BidStart: bidScope.StartBid,
			BidEnd:   bidScope.EndBid,
			Vid:      vid,
		}
		allocRets = append(allocRets, volRet)
	}

	return
}

func (v *volumeMgr) List(ctx context.Context, codeMode codemode.CodeMode) (vids []proto.Vid, volumes []clustermgr.AllocVolumeInfo, err error) {
	span := trace.SpanFromContextSafe(ctx)
	modeInfo, ok := v.modeInfos[codeMode]
	if !ok {
		return nil, nil, errcode.ErrNoAvaliableVolume
	}
	vids = make([]proto.Vid, 0, 128)
	volumes = make([]clustermgr.AllocVolumeInfo, 0, 128)
	vols := modeInfo.volumes.List()
	for _, vol := range vols {
		vol.mu.RLock()
		vids = append(vids, vol.Vid)
		volumes = append(volumes, vol.AllocVolumeInfo)
		vol.mu.RUnlock()
	}
	span.Debugf("[list]code mode: %v, available volumes: %v,count: %v", codeMode, volumes, len(volumes))
	return
}

func (v *volumeMgr) getNextVid(ctx context.Context, vols []*volume, modeInfo *ModeInfo, args *proxy.AllocVolsArgs) (proto.Vid, error) {
	curIdx := int(atomic.AddUint64(&v.preIdx, uint64(1)) % uint64(len(vols)))
	l := len(vols) + curIdx
	for i := curIdx; i < l; i++ {
		idx := i % len(vols)
		if v.modifySpace(ctx, vols[idx], modeInfo, args) {
			return vols[idx].Vid, nil
		}
	}
	return 0, errcode.ErrNoAvailableVolume
}

func (v *volumeMgr) modifySpace(ctx context.Context, volInfo *volume, modeInfo *ModeInfo, args *proxy.AllocVolsArgs) bool {
	span := trace.SpanFromContextSafe(ctx)
	for _, id := range args.Excludes {
		if volInfo.Vid == id {
			return false
		}
	}
	volInfo.mu.Lock()
	if volInfo.Free < args.Fsize || volInfo.deleted {
		span.Warnf("reselect vid: %v, free: %v, size: %v", volInfo.Vid, volInfo.Free, args.Fsize)
		volInfo.mu.Unlock()
		return false
	}
	volInfo.Free -= args.Fsize
	volInfo.Used += args.Fsize
	span.Debugf("selectVid: %v, this vid allocated Size: %v, freeSize: %v, reserve size: %v",
		volInfo.Vid, volInfo.Used, volInfo.Free, v.VolumeReserveSize)
	deleteFlag := false
	if volInfo.Free < uint64(v.VolumeReserveSize) {
		span.Infof("volume is full, remove vid:%v", volInfo.Vid)
		volInfo.deleted = true
		atomic.AddUint64(&modeInfo.totalFree, -volInfo.Free)
		deleteFlag = true
	}
	volInfo.mu.Unlock()
	if deleteFlag {
		modeInfo.volumes.Delete(volInfo.Vid)
	}
	return true
}

func (v *volumeMgr) allocVid(ctx context.Context, args *proxy.AllocVolsArgs) (proto.Vid, error) {
	span := trace.SpanFromContextSafe(ctx)
	modeInfo := v.modeInfos[args.CodeMode]
	if modeInfo == nil {
		return 0, errcode.ErrNoAvaliableVolume
	}
	vols, err := v.getAvailableVols(ctx, args)
	if err != nil {
		return 0, err
	}
	span.Debugf("code mode: %v, available volumes: %v", args.CodeMode, vols)
	vid, err := v.getNextVid(ctx, vols, modeInfo, args)
	if err != nil {
		return 0, err
	}
	if atomic.AddUint64(&modeInfo.totalFree, -args.Fsize) < modeInfo.totalThreshold {
		span.Infof("less than threshold")
		v.allocNotify(ctx, args.CodeMode, v.DefaultAllocVolsNum)
	}
	span.Debugf("code_mode: %v, modeInfo.totalFree: %v, modeInfo.totalThreshold: %v", args.CodeMode,
		atomic.LoadUint64(&modeInfo.totalFree), atomic.LoadUint64(&modeInfo.totalThreshold))
	return vid, nil
}

func (v *volumeMgr) getAvailableVols(ctx context.Context, args *proxy.AllocVolsArgs) (vols []*volume, err error) {
	modeInfo := v.modeInfos[args.CodeMode]
	for _, vid := range args.Discards {
		if vol, ok := modeInfo.volumes.Get(vid); ok {
			vol.mu.Lock()
			if vol.deleted {
				vol.mu.Unlock()
				continue
			}
			atomic.AddUint64(&modeInfo.totalFree, -vol.Free)
			vol.deleted = true
			vol.mu.Unlock()
			modeInfo.volumes.Delete(vid)
		}
	}

	vols = modeInfo.volumes.List()
	if len(vols) == 0 {
		v.allocNotify(ctx, args.CodeMode, v.DefaultAllocVolsNum)
		return nil, errcode.ErrNoAvaliableVolume
	}

	return vols, nil
}

// send message to apply channel, apply volume from CM
func (v *volumeMgr) allocNotify(ctx context.Context, mode codemode.CodeMode, count int) {
	span := trace.SpanFromContextSafe(ctx)
	applyArg := &allocArgs{
		codeMode: mode,
		count:    count,
	}
	// todo bugfix
	if _, ok := v.allocChs[mode]; ok {
		select {
		case v.allocChs[mode] <- applyArg:
			span.Infof("allocNotify {codeMode %s count %v} success", mode.String(), count)
		default:
			span.Infof("the codeMode %s is allocating volume, count: %d", mode.String(), count)
		}
		return
	}
	span.Panicf("the codeMode %v not exist", mode)
}

func (v *volumeMgr) allocVolume(ctx context.Context, args *clustermgr.AllocVolumeArgs) (ret []clustermgr.AllocVolumeInfo,
	err error) {
	span := trace.SpanFromContextSafe(ctx)
	err = retry.ExponentialBackoff(2, 200).On(func() error {
		allocVolumes, err := v.clusterMgr.AllocVolume(ctx, args)
		span.Infof("alloc volume from clusterMgr: %#v, err: %v", allocVolumes, err)
		if err == nil && len(allocVolumes.AllocVolumeInfos) != 0 {
			ret = allocVolumes.AllocVolumeInfos
		}
		return err
	})
	if err != nil {
		return nil, errors.New("allocVolume from clusterMgr error")
	}
	return ret, err
}

func (v *volumeMgr) allocVolumeLoop(mode codemode.CodeMode) {
	for {
		args := <-v.allocChs[mode]
		span, ctx := trace.StartSpanFromContext(context.Background(), "")
		requireCount := args.count
		for {
			allocArg := &clustermgr.AllocVolumeArgs{
				IsInit:   args.isInit,
				CodeMode: args.codeMode,
				Count:    requireCount,
			}
			span.Infof("allocVolumeLoop arguments: %+v", *allocArg)
			volumeRets, err := v.allocVolume(ctx, allocArg)
			if err != nil {
				span.Warnf("alloc volume codemode: %s, err: %v", mode.String(), err)
				time.Sleep(time.Duration(10) * time.Second)
				args.isInit = false
				continue
			}
			for _, vol := range volumeRets {
				allocVolInfo := &volume{
					AllocVolumeInfo: vol,
				}
				v.modeInfos[allocArg.CodeMode].volumes.Put(allocVolInfo)
				atomic.AddUint64(&v.modeInfos[allocArg.CodeMode].totalFree, vol.Free)
			}
			if len(volumeRets) < requireCount {
				span.Warnf("clusterMgr volume num not enough.code_mode: %v, need: %v, got: %v", allocArg.CodeMode,
					requireCount, len(volumeRets))
				requireCount -= len(volumeRets)
				args.isInit = false
				continue
			}
			break
		}
	}
}
