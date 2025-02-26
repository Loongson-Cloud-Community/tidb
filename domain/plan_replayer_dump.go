// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package domain

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/bindinfo"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/statistics/handle"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/printer"
	"github.com/pingcap/tidb/util/sqlexec"
	"go.uber.org/zap"
)

const (
	// PlanReplayerConfigFile indicates config file path for plan replayer
	PlanReplayerConfigFile = "config.toml"
	// PlanReplayerMetaFile meta file path for plan replayer
	PlanReplayerMetaFile = "meta.txt"
	// PlanReplayerVariablesFile indicates for session variables file path for plan replayer
	PlanReplayerVariablesFile = "variables.toml"
	// PlanReplayerTiFlashReplicasFile indicates for table tiflash replica file path for plan replayer
	PlanReplayerTiFlashReplicasFile = "table_tiflash_replica.txt"
	// PlanReplayerSessionBindingFile indicates session binding file path for plan replayer
	PlanReplayerSessionBindingFile = "session_bindings.sql"
	// PlanReplayerGlobalBindingFile indicates global binding file path for plan replayer
	PlanReplayerGlobalBindingFile = "global_bindings.sql"
)

type tableNamePair struct {
	DBName    string
	TableName string
	IsView    bool
}

type tableNameExtractor struct {
	ctx      context.Context
	executor sqlexec.RestrictedSQLExecutor
	is       infoschema.InfoSchema
	curDB    model.CIStr
	names    map[tableNamePair]struct{}
	cteNames map[string]struct{}
	err      error
}

func (tne *tableNameExtractor) Enter(in ast.Node) (ast.Node, bool) {
	if _, ok := in.(*ast.TableName); ok {
		return in, true
	}
	return in, false
}

func (tne *tableNameExtractor) Leave(in ast.Node) (ast.Node, bool) {
	if tne.err != nil {
		return in, true
	}
	if t, ok := in.(*ast.TableName); ok {
		isView, err := tne.handleIsView(t)
		if err != nil {
			tne.err = err
			return in, true
		}
		tp := tableNamePair{DBName: t.Schema.L, TableName: t.Name.L, IsView: isView}
		if tp.DBName == "" {
			tp.DBName = tne.curDB.L
		}
		if _, ok := tne.names[tp]; !ok {
			tne.names[tp] = struct{}{}
		}
	} else if s, ok := in.(*ast.SelectStmt); ok {
		if s.With != nil && len(s.With.CTEs) > 0 {
			for _, cte := range s.With.CTEs {
				tne.cteNames[cte.Name.L] = struct{}{}
			}
		}
	}
	return in, true
}

func (tne *tableNameExtractor) handleIsView(t *ast.TableName) (bool, error) {
	schema := t.Schema
	if schema.L == "" {
		schema = tne.curDB
	}
	table := t.Name
	isView := tne.is.TableIsView(schema, table)
	if !isView {
		return false, nil
	}
	viewTbl, err := tne.is.TableByName(schema, table)
	if err != nil {
		return false, err
	}
	sql := viewTbl.Meta().View.SelectStmt
	node, err := tne.executor.ParseWithParams(tne.ctx, sql)
	if err != nil {
		return false, err
	}
	node.Accept(tne)
	return true, nil
}

// DumpPlanReplayerInfo will dump the information about sqls.
// The files will be organized into the following format:
/*
 |-meta.txt
 |-schema
 |	 |-db1.table1.schema.txt
 |	 |-db2.table2.schema.txt
 |	 |-....
 |-view
 | 	 |-db1.view1.view.txt
 |	 |-db2.view2.view.txt
 |	 |-....
 |-stats
 |   |-stats1.json
 |   |-stats2.json
 |   |-....
 |-config.toml
 |-table_tiflash_replica.txt
 |-variables.toml
 |-bindings.sql
 |-sql
 |   |-sql1.sql
 |   |-sql2.sql
 |	 |-....
 |_explain
     |-explain1.txt
     |-explain2.txt
     |-....
*/
func DumpPlanReplayerInfo(ctx context.Context, sctx sessionctx.Context,
	task *PlanReplayerDumpTask) (err error) {
	zf := task.Zf
	fileName := task.FileName
	sessionVars := task.SessionVars
	execStmts := task.ExecStmts
	zw := zip.NewWriter(zf)
	records := generateRecords(task)
	defer func() {
		err = zw.Close()
		if err != nil {
			logutil.BgLogger().Error("Closing zip writer failed", zap.Error(err), zap.String("filename", fileName))
		}
		err = zf.Close()
		if err != nil {
			logutil.BgLogger().Error("Closing zip file failed", zap.Error(err), zap.String("filename", fileName))
			for i, record := range records {
				record.FailedReason = err.Error()
				records[i] = record
			}
		}
		insertPlanReplayerStatus(ctx, sctx, records)
	}()
	// Dump config
	if err = dumpConfig(zw); err != nil {
		return err
	}

	// Dump meta
	if err = dumpMeta(zw); err != nil {
		return err
	}
	// Retrieve current DB
	dbName := model.NewCIStr(sessionVars.CurrentDB)
	do := GetDomain(sctx)

	// Retrieve all tables
	pairs, err := extractTableNames(ctx, sctx, execStmts, dbName)
	if err != nil {
		return errors.AddStack(fmt.Errorf("plan replayer: invalid SQL text, err: %v", err))
	}

	// Dump Schema and View
	if err = dumpSchemas(sctx, zw, pairs); err != nil {
		return err
	}

	// Dump tables tiflash replicas
	if err = dumpTiFlashReplica(sctx, zw, pairs); err != nil {
		return err
	}

	// Dump stats
	if err = dumpStats(zw, pairs, task.JSONTblStats, do); err != nil {
		return err
	}

	// Dump variables
	if err = dumpVariables(sctx, sessionVars, zw); err != nil {
		return err
	}

	// Dump sql
	if err = dumpSQLs(execStmts, zw); err != nil {
		return err
	}

	// Dump session bindings
	if len(task.SessionBindings) > 0 {
		if err = dumpSessionBindRecords(task.SessionBindings, zw); err != nil {
			return err
		}
	} else {
		if err = dumpSessionBindings(sctx, zw); err != nil {
			return err
		}
	}

	// Dump global bindings
	if err = dumpGlobalBindings(sctx, zw); err != nil {
		return err
	}

	if len(task.EncodedPlan) > 0 {
		return dumpEncodedPlan(sctx, zw, task.EncodedPlan)
	}
	// Dump explain
	return dumpExplain(sctx, zw, execStmts, task.Analyze)
}

func generateRecords(task *PlanReplayerDumpTask) []PlanReplayerStatusRecord {
	records := make([]PlanReplayerStatusRecord, 0)
	if len(task.ExecStmts) > 0 {
		for _, execStmt := range task.ExecStmts {
			records = append(records, PlanReplayerStatusRecord{
				SQLDigest:  task.SQLDigest,
				PlanDigest: task.PlanDigest,
				OriginSQL:  execStmt.Text(),
				Token:      task.FileName,
			})
		}
	}
	return records
}

func dumpConfig(zw *zip.Writer) error {
	cf, err := zw.Create(PlanReplayerConfigFile)
	if err != nil {
		return errors.AddStack(err)
	}
	if err := toml.NewEncoder(cf).Encode(config.GetGlobalConfig()); err != nil {
		return errors.AddStack(err)
	}
	return nil
}

func dumpMeta(zw *zip.Writer) error {
	mt, err := zw.Create(PlanReplayerMetaFile)
	if err != nil {
		return errors.AddStack(err)
	}
	_, err = mt.Write([]byte(printer.GetTiDBInfo()))
	if err != nil {
		return errors.AddStack(err)
	}
	return nil
}

func dumpTiFlashReplica(ctx sessionctx.Context, zw *zip.Writer, pairs map[tableNamePair]struct{}) error {
	bf, err := zw.Create(PlanReplayerTiFlashReplicasFile)
	if err != nil {
		return errors.AddStack(err)
	}
	is := GetDomain(ctx).InfoSchema()
	for pair := range pairs {
		dbName := model.NewCIStr(pair.DBName)
		tableName := model.NewCIStr(pair.TableName)
		t, err := is.TableByName(dbName, tableName)
		if err != nil {
			logutil.BgLogger().Warn("failed to find table info", zap.Error(err),
				zap.String("dbName", dbName.L), zap.String("tableName", tableName.L))
			continue
		}
		if t.Meta().TiFlashReplica != nil && t.Meta().TiFlashReplica.Count > 0 {
			row := []string{
				pair.DBName, pair.TableName, strconv.FormatUint(t.Meta().TiFlashReplica.Count, 10),
			}
			fmt.Fprintf(bf, "%s\n", strings.Join(row, "\t"))
		}
	}
	return nil
}

func dumpSchemas(ctx sessionctx.Context, zw *zip.Writer, pairs map[tableNamePair]struct{}) error {
	for pair := range pairs {
		err := getShowCreateTable(pair, zw, ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

func dumpStats(zw *zip.Writer, pairs map[tableNamePair]struct{}, tblJSONStats map[int64]*handle.JSONTable, do *Domain) error {
	for pair := range pairs {
		if pair.IsView {
			continue
		}
		jsonTbl, err := getStatsForTable(do, tblJSONStats, pair)
		if err != nil {
			return err
		}
		statsFw, err := zw.Create(fmt.Sprintf("stats/%v.%v.json", pair.DBName, pair.TableName))
		if err != nil {
			return errors.AddStack(err)
		}
		data, err := json.Marshal(jsonTbl)
		if err != nil {
			return errors.AddStack(err)
		}
		_, err = statsFw.Write(data)
		if err != nil {
			return errors.AddStack(err)
		}
	}
	return nil
}

func dumpSQLs(execStmts []ast.StmtNode, zw *zip.Writer) error {
	for i, stmtExec := range execStmts {
		zf, err := zw.Create(fmt.Sprintf("sql/sql%v.sql", i))
		if err != nil {
			return err
		}
		_, err = zf.Write([]byte(stmtExec.Text()))
		if err != nil {
			return err
		}
	}
	return nil
}

func dumpVariables(sctx sessionctx.Context, sessionVars *variable.SessionVars, zw *zip.Writer) error {
	varMap := make(map[string]string)
	for _, v := range variable.GetSysVars() {
		if v.IsNoop && !variable.EnableNoopVariables.Load() {
			continue
		}
		if infoschema.SysVarHiddenForSem(sctx, v.Name) {
			continue
		}
		value, err := sessionVars.GetSessionOrGlobalSystemVar(context.Background(), v.Name)
		if err != nil {
			return errors.Trace(err)
		}
		varMap[v.Name] = value
	}
	vf, err := zw.Create(PlanReplayerVariablesFile)
	if err != nil {
		return errors.AddStack(err)
	}
	if err := toml.NewEncoder(vf).Encode(varMap); err != nil {
		return errors.AddStack(err)
	}
	return nil
}

func dumpSessionBindRecords(records []*bindinfo.BindRecord, zw *zip.Writer) error {
	sRows := make([][]string, 0)
	for _, bindData := range records {
		for _, hint := range bindData.Bindings {
			sRows = append(sRows, []string{
				bindData.OriginalSQL,
				hint.BindSQL,
				bindData.Db,
				hint.Status,
				hint.CreateTime.String(),
				hint.UpdateTime.String(),
				hint.Charset,
				hint.Collation,
				hint.Source,
			})
		}
	}
	bf, err := zw.Create(PlanReplayerSessionBindingFile)
	if err != nil {
		return errors.AddStack(err)
	}
	for _, row := range sRows {
		fmt.Fprintf(bf, "%s\n", strings.Join(row, "\t"))
	}
	return nil
}

func dumpSessionBindings(ctx sessionctx.Context, zw *zip.Writer) error {
	recordSets, err := ctx.(sqlexec.SQLExecutor).Execute(context.Background(), "show bindings")
	if err != nil {
		return err
	}
	sRows, err := resultSetToStringSlice(context.Background(), recordSets[0], true)
	if err != nil {
		return err
	}
	bf, err := zw.Create(PlanReplayerSessionBindingFile)
	if err != nil {
		return errors.AddStack(err)
	}
	for _, row := range sRows {
		fmt.Fprintf(bf, "%s\n", strings.Join(row, "\t"))
	}
	if len(recordSets) > 0 {
		if err := recordSets[0].Close(); err != nil {
			return err
		}
	}
	return nil
}

func dumpGlobalBindings(ctx sessionctx.Context, zw *zip.Writer) error {
	recordSets, err := ctx.(sqlexec.SQLExecutor).Execute(context.Background(), "show global bindings")
	if err != nil {
		return err
	}
	sRows, err := resultSetToStringSlice(context.Background(), recordSets[0], false)
	if err != nil {
		return err
	}
	bf, err := zw.Create(PlanReplayerGlobalBindingFile)
	if err != nil {
		return errors.AddStack(err)
	}
	for _, row := range sRows {
		fmt.Fprintf(bf, "%s\n", strings.Join(row, "\t"))
	}
	if len(recordSets) > 0 {
		if err := recordSets[0].Close(); err != nil {
			return err
		}
	}
	return nil
}

func dumpEncodedPlan(ctx sessionctx.Context, zw *zip.Writer, encodedPlan string) error {
	var recordSets []sqlexec.RecordSet
	var err error
	recordSets, err = ctx.(sqlexec.SQLExecutor).Execute(context.Background(), fmt.Sprintf("select tidb_decode_plan('%s')", encodedPlan))
	if err != nil {
		return err
	}
	sRows, err := resultSetToStringSlice(context.Background(), recordSets[0], false)
	if err != nil {
		return err
	}
	fw, err := zw.Create("explain/sql.txt")
	if err != nil {
		return errors.AddStack(err)
	}
	for _, row := range sRows {
		fmt.Fprintf(fw, "%s\n", strings.Join(row, "\t"))
	}
	if len(recordSets) > 0 {
		if err := recordSets[0].Close(); err != nil {
			return err
		}
	}
	return nil
}

func dumpExplain(ctx sessionctx.Context, zw *zip.Writer, execStmts []ast.StmtNode, isAnalyze bool) error {
	for i, stmtExec := range execStmts {
		sql := stmtExec.Text()
		var recordSets []sqlexec.RecordSet
		var err error
		if isAnalyze {
			// Explain analyze
			recordSets, err = ctx.(sqlexec.SQLExecutor).Execute(context.Background(), fmt.Sprintf("explain analyze %s", sql))
			if err != nil {
				return err
			}
		} else {
			// Explain
			recordSets, err = ctx.(sqlexec.SQLExecutor).Execute(context.Background(), fmt.Sprintf("explain %s", sql))
			if err != nil {
				return err
			}
		}
		sRows, err := resultSetToStringSlice(context.Background(), recordSets[0], false)
		if err != nil {
			return err
		}
		fw, err := zw.Create(fmt.Sprintf("explain/sql%v.txt", i))
		if err != nil {
			return errors.AddStack(err)
		}
		for _, row := range sRows {
			fmt.Fprintf(fw, "%s\n", strings.Join(row, "\t"))
		}
		if len(recordSets) > 0 {
			if err := recordSets[0].Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

func extractTableNames(ctx context.Context, sctx sessionctx.Context,
	ExecStmts []ast.StmtNode, curDB model.CIStr) (map[tableNamePair]struct{}, error) {
	tableExtractor := &tableNameExtractor{
		ctx:      ctx,
		executor: sctx.(sqlexec.RestrictedSQLExecutor),
		is:       GetDomain(sctx).InfoSchema(),
		curDB:    curDB,
		names:    make(map[tableNamePair]struct{}),
		cteNames: make(map[string]struct{}),
	}
	for _, execStmt := range ExecStmts {
		execStmt.Accept(tableExtractor)
	}
	if tableExtractor.err != nil {
		return nil, tableExtractor.err
	}
	r := make(map[tableNamePair]struct{})
	for tablePair := range tableExtractor.names {
		if tablePair.IsView {
			r[tablePair] = struct{}{}
			continue
		}
		// remove cte in table names
		_, ok := tableExtractor.cteNames[tablePair.TableName]
		if !ok {
			r[tablePair] = struct{}{}
		}
	}
	return r, nil
}

func getStatsForTable(do *Domain, tblJSONStats map[int64]*handle.JSONTable, pair tableNamePair) (*handle.JSONTable, error) {
	is := do.InfoSchema()
	h := do.StatsHandle()
	tbl, err := is.TableByName(model.NewCIStr(pair.DBName), model.NewCIStr(pair.TableName))
	if err != nil {
		return nil, err
	}
	js, ok := tblJSONStats[tbl.Meta().ID]
	if ok && js != nil {
		return js, nil
	}
	js, err = h.DumpStatsToJSON(pair.DBName, tbl.Meta(), nil, true)
	return js, err
}

func getShowCreateTable(pair tableNamePair, zw *zip.Writer, ctx sessionctx.Context) error {
	recordSets, err := ctx.(sqlexec.SQLExecutor).Execute(context.Background(), fmt.Sprintf("show create table `%v`.`%v`", pair.DBName, pair.TableName))
	if err != nil {
		return err
	}
	sRows, err := resultSetToStringSlice(context.Background(), recordSets[0], false)
	if err != nil {
		return err
	}
	var fw io.Writer
	if pair.IsView {
		fw, err = zw.Create(fmt.Sprintf("view/%v.%v.view.txt", pair.DBName, pair.TableName))
		if err != nil {
			return errors.AddStack(err)
		}
		if len(sRows) == 0 || len(sRows[0]) != 4 {
			return fmt.Errorf("plan replayer: get create view %v.%v failed", pair.DBName, pair.TableName)
		}
	} else {
		fw, err = zw.Create(fmt.Sprintf("schema/%v.%v.schema.txt", pair.DBName, pair.TableName))
		if err != nil {
			return errors.AddStack(err)
		}
		if len(sRows) == 0 || len(sRows[0]) != 2 {
			return fmt.Errorf("plan replayer: get create table %v.%v failed", pair.DBName, pair.TableName)
		}
	}
	fmt.Fprintf(fw, "create database if not exists `%v`; use `%v`;", pair.DBName, pair.DBName)
	fmt.Fprintf(fw, "%s", sRows[0][1])
	if len(recordSets) > 0 {
		if err := recordSets[0].Close(); err != nil {
			return err
		}
	}
	return nil
}

func resultSetToStringSlice(ctx context.Context, rs sqlexec.RecordSet, emptyAsNil bool) ([][]string, error) {
	rows, err := getRows(ctx, rs)
	if err != nil {
		return nil, err
	}
	err = rs.Close()
	if err != nil {
		return nil, err
	}
	sRows := make([][]string, len(rows))
	for i, row := range rows {
		iRow := make([]string, row.Len())
		for j := 0; j < row.Len(); j++ {
			if row.IsNull(j) {
				iRow[j] = "<nil>"
			} else {
				d := row.GetDatum(j, &rs.Fields()[j].Column.FieldType)
				iRow[j], err = d.ToString()
				if err != nil {
					return nil, err
				}
				if len(iRow[j]) < 1 && emptyAsNil {
					iRow[j] = "<nil>"
				}
			}
		}
		sRows[i] = iRow
	}
	return sRows, nil
}

func getRows(ctx context.Context, rs sqlexec.RecordSet) ([]chunk.Row, error) {
	if rs == nil {
		return nil, nil
	}
	var rows []chunk.Row
	req := rs.NewChunk(nil)
	// Must reuse `req` for imitating server.(*clientConn).writeChunks
	for {
		err := rs.Next(ctx, req)
		if err != nil {
			return nil, err
		}
		if req.NumRows() == 0 {
			break
		}

		iter := chunk.NewIterator4Chunk(req.CopyConstruct())
		for row := iter.Begin(); row != iter.End(); row = iter.Next() {
			rows = append(rows, row)
		}
	}
	return rows, nil
}
