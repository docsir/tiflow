// Copyright 2020 PingCAP, Inc.
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

package shardddl

import (
	"context"
	"sync"

	"github.com/pingcap/tidb/parser/model"
	filter "github.com/pingcap/tidb/util/table-filter"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"

	tcontext "github.com/pingcap/tiflow/dm/pkg/context"
	"github.com/pingcap/tiflow/dm/pkg/etcdutil"
	"github.com/pingcap/tiflow/dm/pkg/log"
	"github.com/pingcap/tiflow/dm/pkg/shardddl/optimism"
)

// Optimist used to coordinate the shard DDL migration in optimism mode.
type Optimist struct {
	mu sync.RWMutex

	logger log.Logger
	cli    *clientv3.Client
	task   string
	source string

	tables optimism.SourceTables

	// the shard DDL info which is pending to handle.
	pendingInfo *optimism.Info
	// the shard DDL lock operation which is pending to handle.
	pendingOp *optimism.Operation
}

// NewOptimist creates a new Optimist instance.
func NewOptimist(pLogger *log.Logger, cli *clientv3.Client, task, source string) *Optimist {
	return &Optimist{
		logger: pLogger.WithFields(zap.String("component", "shard DDL optimist")),
		cli:    cli,
		task:   task,
		source: source,
	}
}

// Init initializes the optimist with source tables.
// NOTE: this will PUT the initial source tables into etcd (and overwrite any previous existing tables).
// NOTE: we do not remove source tables for `stop-task` now, may need to handle it for `remove-meta`.
func (o *Optimist) Init(sourceTables map[string]map[string]map[string]map[string]struct{}) error {
	o.tables = optimism.NewSourceTables(o.task, o.source)
	for downSchema, downTables := range sourceTables {
		for downTable, upSchemas := range downTables {
			for upSchema, upTables := range upSchemas {
				for upTable := range upTables {
					o.tables.AddTable(upSchema, upTable, downSchema, downTable)
				}
			}
		}
	}
	_, err := optimism.PutSourceTables(o.cli, o.tables)
	return err
}

// Tables clone and return tables
// first one is sourceTable, second one is targetTable.
func (o *Optimist) Tables() [][]filter.Table {
	o.mu.Lock()
	defer o.mu.Unlock()

	tbls := make([][]filter.Table, 0)
	for downSchema, downTables := range o.tables.Tables {
		for downTable, upSchemas := range downTables {
			for upSchema, upTables := range upSchemas {
				for upTable := range upTables {
					tbls = append(tbls, []filter.Table{{Schema: upSchema, Name: upTable}, {Schema: downSchema, Name: downTable}})
				}
			}
		}
	}
	return tbls
}

// Reset resets the internal state of the optimist.
func (o *Optimist) Reset() {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.pendingInfo = nil
	o.pendingOp = nil
}

// ConstructInfo constructs a shard DDL info.
func (o *Optimist) ConstructInfo(upSchema, upTable, downSchema, downTable string,
	ddls []string, tiBefore *model.TableInfo, tisAfter []*model.TableInfo,
) optimism.Info {
	return optimism.NewInfo(o.task, o.source, upSchema, upTable, downSchema, downTable, ddls, tiBefore, tisAfter)
}

// PutInfo puts the shard DDL info into etcd and returns the revision.
func (o *Optimist) PutInfo(info optimism.Info) (int64, error) {
	rev, err := optimism.PutInfo(o.cli, info)
	if err != nil {
		return 0, err
	}

	o.mu.Lock()
	o.pendingInfo = &info
	o.mu.Unlock()

	return rev, nil
}

// AddTable adds the table for the info into source tables,
// this is often called for `CREATE TABLE`.
func (o *Optimist) AddTable(info optimism.Info) (int64, error) {
	o.tables.AddTable(info.UpSchema, info.UpTable, info.DownSchema, info.DownTable)
	return optimism.PutSourceTables(o.cli, o.tables)
}

// RemoveTable removes the table for the info from source tables,
// this is often called for `DROP TABLE`.
func (o *Optimist) RemoveTable(info optimism.Info) (int64, error) {
	o.tables.RemoveTable(info.UpSchema, info.UpTable, info.DownSchema, info.DownTable)
	return optimism.PutSourceTables(o.cli, o.tables)
}

// GetOperation gets the shard DDL lock operation relative to the shard DDL info.
func (o *Optimist) GetOperation(ctx context.Context, info optimism.Info, rev int64) (optimism.Operation, error) {
	ctx2, cancel2 := context.WithCancel(ctx)
	defer cancel2()

	ch := make(chan optimism.Operation, 1)
	errCh := make(chan error, 1)
	go optimism.WatchOperationPut(ctx2, o.cli, o.task, o.source, info.UpSchema, info.UpTable, rev, ch, errCh)

	select {
	case op := <-ch:
		o.mu.Lock()
		o.pendingOp = &op
		o.mu.Unlock()
		return op, nil
	case err := <-errCh:
		return optimism.Operation{}, err
	case <-ctx.Done():
		return optimism.Operation{}, ctx.Err()
	}
}

// DoneOperation marks the shard DDL lock operation as done.
func (o *Optimist) DoneOperation(op optimism.Operation) error {
	op.Done = true
	_, _, err := etcdutil.DoTxnWithRepeatable(o.cli, func(_ *tcontext.Context, cli *clientv3.Client) (interface{}, error) {
		_, _, err := optimism.PutOperation(cli, false, op, 0)
		return nil, err
	})
	if err != nil {
		return err
	}

	o.mu.Lock()
	o.pendingInfo = nil
	o.pendingOp = nil
	o.mu.Unlock()

	return nil
}

// PendingInfo returns the shard DDL info which is pending to handle.
func (o *Optimist) PendingInfo() *optimism.Info {
	o.mu.RLock()
	defer o.mu.RUnlock()

	if o.pendingInfo == nil {
		return nil
	}
	info := *o.pendingInfo
	return &info
}

// PendingOperation returns the shard DDL lock operation which is pending to handle.
func (o *Optimist) PendingOperation() *optimism.Operation {
	o.mu.RLock()
	defer o.mu.RUnlock()

	if o.pendingOp == nil {
		return nil
	}
	op := *o.pendingOp
	return &op
}

// CheckPersistentData check and fix the persistent data.
//
// NOTE: currently this function is not used because user will meet error at early version
// if set unsupported case-sensitive.
func (o *Optimist) CheckPersistentData(source string, schemas map[string]string, tables map[string]map[string]string) error {
	if o.cli == nil {
		return nil
	}
	err := optimism.CheckSourceTables(o.cli, source, schemas, tables)
	if err != nil {
		return err
	}

	err = optimism.CheckDDLInfos(o.cli, source, schemas, tables)
	if err != nil {
		return err
	}

	err = optimism.CheckOperations(o.cli, source, schemas, tables)
	if err != nil {
		return err
	}

	return optimism.CheckColumns(o.cli, source, schemas, tables)
}
