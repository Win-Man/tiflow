// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package gc

import (
	"context"
	"time"

	"github.com/pingcap/failpoint"
	"github.com/pingcap/log"
	"github.com/pingcap/tiflow/cdc/model"
	"github.com/pingcap/tiflow/pkg/config"
	cerror "github.com/pingcap/tiflow/pkg/errors"
	"github.com/pingcap/tiflow/pkg/pdutil"
	"github.com/tikv/client-go/v2/oracle"
	pd "github.com/tikv/pd/client"
	"go.uber.org/zap"
)

// gcTTL is the duration during which data related to a
// failed feed will be retained, and beyond which point the data will be deleted
// by garbage collection.
const gcTTL = 24 * time.Hour

// gcSafepointUpdateInterval is the minimum interval that CDC can update gc safepoint
var gcSafepointUpdateInterval = 1 * time.Minute

// Manager is an interface for gc manager
type Manager interface {
	// TryUpdateGCSafePoint tries to update TiCDC service GC safepoint.
	// Manager may skip update when it thinks it is too frequent.
	// Set `forceUpdate` to force Manager update.
	TryUpdateGCSafePoint(ctx context.Context, checkpointTs model.Ts, forceUpdate bool) error
	CheckStaleCheckpointTs(ctx context.Context, changefeedID model.ChangeFeedID, checkpointTs model.Ts) error
	// IgnoreFailedChangeFeed verifies whether a failed changefeed should be
	// disregarded. When calculating the GC safepoint of the related upstream,
	IgnoreFailedChangeFeed(checkpointTs uint64) bool
}

type gcManager struct {
	gcServiceID string
	pdClient    pd.Client
	pdClock     pdutil.Clock
	gcTTL       int64

	lastUpdatedTime   time.Time
	lastSucceededTime time.Time
	lastSafePointTs   uint64
}

// NewManager creates a new Manager.
func NewManager(gcServiceID string, pdClient pd.Client, pdClock pdutil.Clock) Manager {
	serverConfig := config.GetGlobalServerConfig()
	failpoint.Inject("InjectGcSafepointUpdateInterval", func(val failpoint.Value) {
		gcSafepointUpdateInterval = time.Duration(val.(int) * int(time.Millisecond))
	})
	return &gcManager{
		gcServiceID:       gcServiceID,
		pdClient:          pdClient,
		pdClock:           pdClock,
		lastSucceededTime: time.Now(),
		gcTTL:             serverConfig.GcTTL,
	}
}

func (m *gcManager) TryUpdateGCSafePoint(
	ctx context.Context, checkpointTs model.Ts, forceUpdate bool,
) error {
	if time.Since(m.lastUpdatedTime) < gcSafepointUpdateInterval && !forceUpdate {
		return nil
	}
	m.lastUpdatedTime = time.Now()

	actual, err := SetServiceGCSafepoint(
		ctx, m.pdClient, m.gcServiceID, m.gcTTL, checkpointTs)
	if err != nil {
		log.Warn("updateGCSafePoint failed",
			zap.Uint64("safePointTs", checkpointTs),
			zap.Error(err))
		if time.Since(m.lastSucceededTime) >= time.Second*time.Duration(m.gcTTL) {
			return cerror.ErrUpdateServiceSafepointFailed.Wrap(err)
		}
		return nil
	}
	failpoint.Inject("InjectActualGCSafePoint", func(val failpoint.Value) {
		actual = uint64(val.(int))
	})
	if actual == checkpointTs {
		log.Info("update gc safe point success", zap.Uint64("gcSafePointTs", checkpointTs))
	}
	if actual > checkpointTs {
		log.Warn("update gc safe point failed, the gc safe point is larger than checkpointTs",
			zap.Uint64("actual", actual), zap.Uint64("checkpointTs", checkpointTs))
	}
	m.lastSafePointTs = actual
	m.lastSucceededTime = time.Now()
	return nil
}

func (m *gcManager) CheckStaleCheckpointTs(
	ctx context.Context, changefeedID model.ChangeFeedID, checkpointTs model.Ts,
) error {
	gcSafepointUpperBound := checkpointTs - 1
	// if there is another service gc point less than the min checkpoint ts.
	if gcSafepointUpperBound < m.lastSafePointTs {
		return cerror.ErrSnapshotLostByGC.
			GenWithStackByArgs(
				checkpointTs,
				m.lastSafePointTs,
			)
	}
	return nil
}

func (m *gcManager) IgnoreFailedChangeFeed(
	checkpointTs uint64,
) bool {
	pdTime, err := m.pdClock.CurrentTime()
	if err != nil {
		log.Warn("failed to get ts",
			zap.String("GcManagerID", m.gcServiceID),
			zap.Error(err),
		)
		return false
	}
	// ignore the changefeed if its current checkpoint TS is earlier
	// than the (currentPDTso - failedFeedDataRetentionTime).
	gcSafepointUpperBound := checkpointTs - 1
	return pdTime.Sub(
		oracle.GetTimeFromTS(gcSafepointUpperBound),
	) > gcTTL
}
