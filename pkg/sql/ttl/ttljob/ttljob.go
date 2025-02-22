// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package ttljob

import (
	"context"
	"math"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/colinfo"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/physicalplan"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqltelemetry"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
)

var (
	defaultSelectBatchSize = settings.RegisterIntSetting(
		settings.TenantWritable,
		"sql.ttl.default_select_batch_size",
		"default amount of rows to select in a single query during a TTL job",
		500,
		settings.PositiveInt,
	).WithPublic()
	defaultDeleteBatchSize = settings.RegisterIntSetting(
		settings.TenantWritable,
		"sql.ttl.default_delete_batch_size",
		"default amount of rows to delete in a single query during a TTL job",
		100,
		settings.PositiveInt,
	).WithPublic()
	defaultRangeConcurrency = settings.RegisterIntSetting(
		settings.TenantWritable,
		"sql.ttl.default_range_concurrency",
		"default amount of ranges to process at once during a TTL delete",
		1,
		settings.PositiveInt,
	).WithPublic()
	defaultDeleteRateLimit = settings.RegisterIntSetting(
		settings.TenantWritable,
		"sql.ttl.default_delete_rate_limit",
		"default delete rate limit for all TTL jobs. Use 0 to signify no rate limit.",
		0,
		settings.NonNegativeInt,
	).WithPublic()

	jobEnabled = settings.RegisterBoolSetting(
		settings.TenantWritable,
		"sql.ttl.job.enabled",
		"whether the TTL job is enabled",
		true,
	).WithPublic()
)

type rowLevelTTLResumer struct {
	job *jobs.Job
	st  *cluster.Settings
}

var _ jobs.Resumer = (*rowLevelTTLResumer)(nil)

// Resume implements the jobs.Resumer interface.
func (t rowLevelTTLResumer) Resume(ctx context.Context, execCtx interface{}) error {
	jobExecCtx := execCtx.(sql.JobExecContext)
	execCfg := jobExecCtx.ExecCfg()
	db := execCfg.DB
	descsCol := jobExecCtx.ExtendedEvalContext().Descs

	settingsValues := execCfg.SV()
	if err := checkEnabled(settingsValues); err != nil {
		return err
	}

	telemetry.Inc(sqltelemetry.RowLevelTTLExecuted)

	var knobs sql.TTLTestingKnobs
	if ttlKnobs := execCfg.TTLTestingKnobs; ttlKnobs != nil {
		knobs = *ttlKnobs
	}

	details := t.job.Details().(jobspb.RowLevelTTLDetails)

	aostDuration := -time.Second * 30
	if knobs.AOSTDuration != nil {
		aostDuration = *knobs.AOSTDuration
	}
	aost := details.Cutoff.Add(aostDuration)

	var rowLevelTTL catpb.RowLevelTTL
	var relationName string
	var entirePKSpan roachpb.Span
	if err := db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		desc, err := descsCol.GetImmutableTableByID(
			ctx,
			txn,
			details.TableID,
			tree.ObjectLookupFlagsWithRequired(),
		)
		if err != nil {
			return err
		}
		// If the AOST timestamp is before the latest descriptor timestamp, exit
		// early as the delete will not work.
		if desc.GetModificationTime().GoTime().After(aost) {
			return errors.Newf(
				"found a recent schema change on the table at %s, aborting",
				desc.GetModificationTime().GoTime().Format(time.RFC3339),
			)
		}

		if !desc.HasRowLevelTTL() {
			return errors.Newf("unable to find TTL on table %s", desc.GetName())
		}

		rowLevelTTL = *desc.GetRowLevelTTL()

		if rowLevelTTL.Pause {
			return errors.Newf("ttl jobs on table %s are currently paused", tree.Name(desc.GetName()))
		}

		tn, err := descs.GetTableNameByDesc(ctx, txn, descsCol, desc)
		if err != nil {
			return errors.Wrapf(err, "error fetching table relation name for TTL")
		}
		relationName = tn.FQString()

		entirePKSpan = desc.PrimaryIndexSpan(execCfg.Codec)
		return nil
	}); err != nil {
		return err
	}

	ttlExpr := colinfo.DefaultTTLExpirationExpr
	if rowLevelTTL.HasExpirationExpr() {
		ttlExpr = "(" + rowLevelTTL.ExpirationExpr + ")"
	}

	labelMetrics := rowLevelTTL.LabelMetrics
	group := ctxgroup.WithContext(ctx)
	err := func() error {
		statsCloseCh := make(chan struct{})
		defer close(statsCloseCh)
		if rowLevelTTL.RowStatsPollInterval != 0 {

			metrics := execCfg.JobRegistry.MetricsStruct().RowLevelTTL.(*RowLevelTTLAggMetrics).loadMetrics(
				labelMetrics,
				relationName,
			)

			group.GoCtx(func(ctx context.Context) error {

				handleError := func(err error) error {
					if knobs.ReturnStatsError {
						return err
					}
					log.Warningf(ctx, "failed to get statistics for table id %d: %s", details.TableID, err)
					return nil
				}

				// Do once initially to ensure we have some base statistics.
				err := metrics.fetchStatistics(ctx, execCfg, relationName, details, aostDuration, ttlExpr)
				if err := handleError(err); err != nil {
					return err
				}
				// Wait until poll interval is reached, or early exit when we are done
				// with the TTL job.
				for {
					select {
					case <-statsCloseCh:
						return nil
					case <-time.After(rowLevelTTL.RowStatsPollInterval):
						err := metrics.fetchStatistics(ctx, execCfg, relationName, details, aostDuration, ttlExpr)
						if err := handleError(err); err != nil {
							return err
						}
					}
				}
			})
		}

		distSQLPlanner := jobExecCtx.DistSQLPlanner()
		evalCtx := jobExecCtx.ExtendedEvalContext()

		// We don't return the compatible nodes here since PartitionSpans will
		// filter out incompatible nodes.
		planCtx, _, err := distSQLPlanner.SetupAllNodesPlanning(ctx, evalCtx, execCfg)
		if err != nil {
			return err
		}
		spanPartitions, err := distSQLPlanner.PartitionSpans(ctx, planCtx, []roachpb.Span{entirePKSpan})
		if err != nil {
			return err
		}
		if knobs.RequireMultipleSpanPartitions && len(spanPartitions) == 0 {
			return errors.New("multiple span partitions required")
		}

		sqlInstanceIDToTTLSpec := make(map[base.SQLInstanceID]*execinfrapb.TTLSpec)
		for _, spanPartition := range spanPartitions {
			ttlSpec := &execinfrapb.TTLSpec{
				JobID:                       t.job.ID(),
				RowLevelTTLDetails:          details,
				AOST:                        aost,
				TTLExpr:                     ttlExpr,
				Spans:                       spanPartition.Spans,
				RangeConcurrency:            getRangeConcurrency(settingsValues, rowLevelTTL),
				SelectBatchSize:             getSelectBatchSize(settingsValues, rowLevelTTL),
				DeleteBatchSize:             getDeleteBatchSize(settingsValues, rowLevelTTL),
				DeleteRateLimit:             getDeleteRateLimit(settingsValues, rowLevelTTL),
				LabelMetrics:                rowLevelTTL.LabelMetrics,
				PreDeleteChangeTableVersion: knobs.PreDeleteChangeTableVersion,
				PreSelectStatement:          knobs.PreSelectStatement,
			}
			sqlInstanceIDToTTLSpec[spanPartition.SQLInstanceID] = ttlSpec
		}

		// Setup a one-stage plan with one proc per input spec.
		processorCorePlacements := make([]physicalplan.ProcessorCorePlacement, len(sqlInstanceIDToTTLSpec))
		i := 0
		for sqlInstanceID, ttlSpec := range sqlInstanceIDToTTLSpec {
			processorCorePlacements[i].SQLInstanceID = sqlInstanceID
			processorCorePlacements[i].Core.Ttl = ttlSpec
			i++
		}

		physicalPlan := planCtx.NewPhysicalPlan()
		// Job progress is updated inside ttlProcessor, so we
		// have an empty result stream.
		physicalPlan.AddNoInputStage(
			processorCorePlacements,
			execinfrapb.PostProcessSpec{},
			[]*types.T{},
			execinfrapb.Ordering{},
		)
		physicalPlan.PlanToStreamColMap = []int{}

		distSQLPlanner.FinalizePlan(planCtx, physicalPlan)

		metadataCallbackWriter := sql.NewMetadataOnlyMetadataCallbackWriter()

		distSQLReceiver := sql.MakeDistSQLReceiver(
			ctx,
			metadataCallbackWriter,
			tree.Rows,
			execCfg.RangeDescriptorCache,
			nil, /* txn */
			nil, /* clockUpdater */
			evalCtx.Tracing,
			execCfg.ContentionRegistry,
			nil, /* testingPushCallback */
		)
		defer distSQLReceiver.Release()

		// Copy the evalCtx, as dsp.Run() might change it.
		evalCtxCopy := *evalCtx
		cleanup := distSQLPlanner.Run(
			ctx,
			planCtx,
			nil, /* txn */
			physicalPlan,
			distSQLReceiver,
			&evalCtxCopy,
			nil, /* finishedSetupFn */
		)
		defer cleanup()

		return metadataCallbackWriter.Err()
	}()
	if err != nil {
		return err
	}

	return group.Wait()
}

func checkEnabled(settingsValues *settings.Values) error {
	if enabled := jobEnabled.Get(settingsValues); !enabled {
		return errors.Newf(
			"ttl jobs are currently disabled by CLUSTER SETTING %s",
			jobEnabled.Key(),
		)
	}
	return nil
}

func getSelectBatchSize(sv *settings.Values, ttl catpb.RowLevelTTL) int64 {
	bs := ttl.SelectBatchSize
	if bs == 0 {
		bs = defaultSelectBatchSize.Get(sv)
	}
	return bs
}

func getDeleteBatchSize(sv *settings.Values, ttl catpb.RowLevelTTL) int64 {
	bs := ttl.DeleteBatchSize
	if bs == 0 {
		bs = defaultDeleteBatchSize.Get(sv)
	}
	return bs
}

func getRangeConcurrency(sv *settings.Values, ttl catpb.RowLevelTTL) int64 {
	rc := ttl.RangeConcurrency
	if rc == 0 {
		rc = defaultRangeConcurrency.Get(sv)
	}
	return rc
}

func getDeleteRateLimit(sv *settings.Values, ttl catpb.RowLevelTTL) int64 {
	rl := ttl.DeleteRateLimit
	if rl == 0 {
		rl = defaultDeleteRateLimit.Get(sv)
	}
	// Put the maximum tokens possible if there is no rate limit.
	if rl == 0 {
		rl = math.MaxInt64
	}
	return rl
}

// OnFailOrCancel implements the jobs.Resumer interface.
func (t rowLevelTTLResumer) OnFailOrCancel(
	ctx context.Context, execCtx interface{}, _ error,
) error {
	return nil
}

func init() {
	jobs.RegisterConstructor(jobspb.TypeRowLevelTTL, func(job *jobs.Job, settings *cluster.Settings) jobs.Resumer {
		return &rowLevelTTLResumer{
			job: job,
			st:  settings,
		}
	}, jobs.UsesTenantCostControl)
}
