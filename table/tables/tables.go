// Copyright 2013 The ql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSES/QL-LICENSE file.

// Copyright 2015 PingCAP, Inc.
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

package tables

import (
	"strings"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/tidb/context"
	"github.com/pingcap/tidb/evaluator"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta/autoid"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/terror"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/types"
)

// Table implements table.Table interface.
type Table struct {
	ID      int64
	Name    model.CIStr
	Columns []*table.Column

	publicColumns   []*table.Column
	writableColumns []*table.Column
	indices         []table.Index
	recordPrefix    kv.Key
	indexPrefix     kv.Key
	alloc           autoid.Allocator
	meta            *model.TableInfo
}

// MockTableFromMeta only serves for test.
func MockTableFromMeta(tableInfo *model.TableInfo) table.Table {
	return &Table{ID: 0, meta: tableInfo}
}

// TableFromMeta creates a Table instance from model.TableInfo.
func TableFromMeta(alloc autoid.Allocator, tblInfo *model.TableInfo) (table.Table, error) {
	if tblInfo.State == model.StateNone {
		return nil, table.ErrTableStateCantNone.Gen("table %s can't be in none state", tblInfo.Name)
	}

	columns := make([]*table.Column, 0, len(tblInfo.Columns))
	for _, colInfo := range tblInfo.Columns {
		if colInfo.State == model.StateNone {
			return nil, table.ErrColumnStateCantNone.Gen("column %s can't be in none state", colInfo.Name)
		}

		col := &table.Column{ColumnInfo: *colInfo}
		columns = append(columns, col)
	}

	t := newTable(tblInfo.ID, columns, alloc)

	for _, idxInfo := range tblInfo.Indices {
		if idxInfo.State == model.StateNone {
			return nil, table.ErrIndexStateCantNone.Gen("index %s can't be in none state", idxInfo.Name)
		}

		idx := NewIndex(tblInfo, idxInfo)
		t.indices = append(t.indices, idx)
	}

	t.meta = tblInfo
	return t, nil
}

// newTable constructs a Table instance.
func newTable(tableID int64, cols []*table.Column, alloc autoid.Allocator) *Table {
	t := &Table{
		ID:           tableID,
		recordPrefix: tablecodec.GenTableRecordPrefix(tableID),
		indexPrefix:  tablecodec.GenTableIndexPrefix(tableID),
		alloc:        alloc,
		Columns:      cols,
	}

	t.publicColumns = t.Cols()
	t.writableColumns = t.writableCols()
	return t
}

// Indices implements table.Table Indices interface.
func (t *Table) Indices() []table.Index {
	return t.indices
}

// Meta implements table.Table Meta interface.
func (t *Table) Meta() *model.TableInfo {
	return t.meta
}

// Cols implements table.Table Cols interface.
func (t *Table) Cols() []*table.Column {
	if len(t.publicColumns) > 0 {
		return t.publicColumns
	}

	t.publicColumns = make([]*table.Column, 0, len(t.Columns))
	for _, col := range t.Columns {
		if col.State == model.StatePublic {
			t.publicColumns = append(t.publicColumns, col)
		}
	}

	return t.publicColumns
}

func (t *Table) writableCols() []*table.Column {
	if len(t.writableColumns) > 0 {
		return t.writableColumns
	}

	t.writableColumns = make([]*table.Column, 0, len(t.Columns))
	for _, col := range t.Columns {
		if col.State == model.StateDeleteOnly || col.State == model.StateDeleteReorganization {
			continue
		}

		t.writableColumns = append(t.writableColumns, col)
	}

	return t.writableColumns
}

// RecordPrefix implements table.Table RecordPrefix interface.
func (t *Table) RecordPrefix() kv.Key {
	return t.recordPrefix
}

// IndexPrefix implements table.Table IndexPrefix interface.
func (t *Table) IndexPrefix() kv.Key {
	return t.indexPrefix
}

// RecordKey implements table.Table RecordKey interface.
func (t *Table) RecordKey(h int64, col *table.Column) kv.Key {
	colID := int64(0)
	if col != nil {
		colID = col.ID
	}
	return tablecodec.EncodeRecordKey(t.recordPrefix, h, colID)
}

// FirstKey implements table.Table FirstKey interface.
func (t *Table) FirstKey() kv.Key {
	return t.RecordKey(0, nil)
}

// Truncate implements table.Table Truncate interface.
func (t *Table) Truncate(ctx context.Context) error {
	txn, err := ctx.GetTxn(false)
	if err != nil {
		return errors.Trace(err)
	}
	err = util.DelKeyWithPrefix(txn, t.RecordPrefix())
	if err != nil {
		return errors.Trace(err)
	}
	return util.DelKeyWithPrefix(txn, t.IndexPrefix())
}

// UpdateRecord implements table.Table UpdateRecord interface.
func (t *Table) UpdateRecord(ctx context.Context, h int64, oldData []types.Datum, newData []types.Datum, touched map[int]bool) error {
	// We should check whether this table has on update column which state is write only.
	currentData := make([]types.Datum, len(t.writableCols()))
	copy(currentData, newData)

	// If they are not set, and other data are changed, they will be updated by current timestamp too.
	err := t.setOnUpdateData(ctx, touched, currentData)
	if err != nil {
		return errors.Trace(err)
	}

	txn, err := ctx.GetTxn(false)
	if err != nil {
		return errors.Trace(err)
	}

	bs := kv.NewBufferStore(txn)
	defer bs.Release()

	// set new value
	if err = t.setNewData(bs, h, touched, currentData); err != nil {
		return errors.Trace(err)
	}

	// rebuild index
	if err = t.rebuildIndices(bs, h, touched, oldData, currentData); err != nil {
		return errors.Trace(err)
	}

	err = bs.SaveTo(txn)
	if err != nil {
		return errors.Trace(err)
	}

	return nil
}

func (t *Table) setOnUpdateData(ctx context.Context, touched map[int]bool, data []types.Datum) error {
	ucols := table.FindOnUpdateCols(t.writableCols())
	for _, col := range ucols {
		if !touched[col.Offset] {
			value, err := evaluator.GetTimeValue(ctx, evaluator.CurrentTimestamp, col.Tp, col.Decimal)
			if err != nil {
				return errors.Trace(err)
			}

			data[col.Offset] = value
			touched[col.Offset] = true
		}
	}
	return nil
}
func (t *Table) setNewData(rm kv.RetrieverMutator, h int64, touched map[int]bool, data []types.Datum) error {
	for _, col := range t.Cols() {
		if !touched[col.Offset] {
			continue
		}

		k := t.RecordKey(h, col)
		if err := SetColValue(rm, k, data[col.Offset]); err != nil {
			return errors.Trace(err)
		}
	}

	return nil
}

func (t *Table) rebuildIndices(rm kv.RetrieverMutator, h int64, touched map[int]bool, oldData []types.Datum, newData []types.Datum) error {
	for _, idx := range t.Indices() {
		idxTouched := false
		for _, ic := range idx.Meta().Columns {
			if touched[ic.Offset] {
				idxTouched = true
				break
			}
		}
		if !idxTouched {
			continue
		}

		oldVs, err := idx.FetchValues(oldData)
		if err != nil {
			return errors.Trace(err)
		}

		if t.removeRowIndex(rm, h, oldVs, idx); err != nil {
			return errors.Trace(err)
		}

		newVs, err := idx.FetchValues(newData)
		if err != nil {
			return errors.Trace(err)
		}

		if err := t.buildIndexForRow(rm, h, newVs, idx); err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

// AddRecord implements table.Table AddRecord interface.
func (t *Table) AddRecord(ctx context.Context, r []types.Datum) (recordID int64, err error) {
	var hasRecordID bool
	for _, col := range t.Cols() {
		if col.IsPKHandleColumn(t.meta) {
			recordID = r[col.Offset].GetInt64()
			hasRecordID = true
			break
		}
	}
	if !hasRecordID {
		recordID, err = t.alloc.Alloc(t.ID)
		if err != nil {
			return 0, errors.Trace(err)
		}
	}
	txn, err := ctx.GetTxn(false)
	if err != nil {
		return 0, errors.Trace(err)
	}
	bs := kv.NewBufferStore(txn)
	defer bs.Release()
	// Insert new entries into indices.
	h, err := t.addIndices(ctx, recordID, r, bs)
	if err != nil {
		return h, errors.Trace(err)
	}

	if err = t.LockRow(ctx, recordID, false); err != nil {
		return 0, errors.Trace(err)
	}
	// Set public and write only column value.
	for _, col := range t.writableCols() {
		if col.IsPKHandleColumn(t.meta) {
			continue
		}
		if col.DefaultValue == nil && r[col.Offset].IsNull() {
			// Save storage space by not storing null value.
			continue
		}
		var value types.Datum
		if col.State == model.StateWriteOnly || col.State == model.StateWriteReorganization {
			// if col is in write only or write reorganization state, we must add it with its default value.
			value, _, err = table.GetColDefaultValue(ctx, &col.ColumnInfo)
			if err != nil {
				return 0, errors.Trace(err)
			}
			value, err = table.CastValue(ctx, value, col)
			if err != nil {
				return 0, errors.Trace(err)
			}
		} else {
			value = r[col.Offset]
		}

		key := t.RecordKey(recordID, col)
		err = SetColValue(txn, key, value)
		if err != nil {
			return 0, errors.Trace(err)
		}
	}
	if err = bs.SaveTo(txn); err != nil {
		return 0, errors.Trace(err)
	}

	variable.GetSessionVars(ctx).AddAffectedRows(1)
	return recordID, nil
}

// Generate index content string representation.
func (t *Table) genIndexKeyStr(colVals []types.Datum) (string, error) {
	// Pass pre-composed error to txn.
	strVals := make([]string, 0, len(colVals))
	for _, cv := range colVals {
		cvs := "NULL"
		var err error
		if !cv.IsNull() {
			cvs, err = types.ToString(cv.GetValue())
			if err != nil {
				return "", errors.Trace(err)
			}
		}
		strVals = append(strVals, cvs)
	}
	return strings.Join(strVals, "-"), nil
}

// Add data into indices.
func (t *Table) addIndices(ctx context.Context, recordID int64, r []types.Datum, bs *kv.BufferStore) (int64, error) {
	txn, err := ctx.GetTxn(false)
	if err != nil {
		return 0, errors.Trace(err)
	}
	// Clean up lazy check error environment
	defer txn.DelOption(kv.PresumeKeyNotExistsError)
	if t.meta.PKIsHandle {
		// Check key exists.
		recordKey := t.RecordKey(recordID, nil)
		e := kv.ErrKeyExists.Gen("Duplicate entry '%d' for key 'PRIMARY'", recordID)
		txn.SetOption(kv.PresumeKeyNotExistsError, e)
		_, err = txn.Get(recordKey)
		if err == nil {
			return recordID, errors.Trace(e)
		} else if !terror.ErrorEqual(err, kv.ErrNotExist) {
			return 0, errors.Trace(err)
		}
		txn.DelOption(kv.PresumeKeyNotExistsError)
	}

	for _, v := range t.indices {
		if v == nil || v.Meta().State == model.StateDeleteOnly || v.Meta().State == model.StateDeleteReorganization {
			// if index is in delete only or delete reorganization state, we can't add it.
			continue
		}
		colVals, _ := v.FetchValues(r)
		var dupKeyErr error
		if v.Meta().Unique || v.Meta().Primary {
			entryKey, err1 := t.genIndexKeyStr(colVals)
			if err1 != nil {
				return 0, errors.Trace(err1)
			}
			dupKeyErr = kv.ErrKeyExists.Gen("Duplicate entry '%s' for key '%s'", entryKey, v.Meta().Name)
			txn.SetOption(kv.PresumeKeyNotExistsError, dupKeyErr)
		}
		if err = v.Create(bs, colVals, recordID); err != nil {
			if terror.ErrorEqual(err, kv.ErrKeyExists) {
				// Get the duplicate row handle
				// For insert on duplicate syntax, we should update the row
				iter, _, err1 := v.Seek(bs, colVals)
				if err1 != nil {
					return 0, errors.Trace(err1)
				}
				_, h, err1 := iter.Next()
				if err1 != nil {
					return 0, errors.Trace(err1)
				}
				return h, errors.Trace(dupKeyErr)
			}
			return 0, errors.Trace(err)
		}
		txn.DelOption(kv.PresumeKeyNotExistsError)
	}
	return 0, nil
}

// RowWithCols implements table.Table RowWithCols interface.
func (t *Table) RowWithCols(ctx context.Context, h int64, cols []*table.Column) ([]types.Datum, error) {
	txn, err := ctx.GetTxn(false)
	if err != nil {
		return nil, errors.Trace(err)
	}
	v := make([]types.Datum, len(cols))
	for i, col := range cols {
		if col == nil {
			continue
		}
		if col.State != model.StatePublic {
			return nil, table.ErrColumnStateNonPublic.Gen("Cannot use none public column - %v", cols)
		}
		if col.IsPKHandleColumn(t.meta) {
			if mysql.HasUnsignedFlag(col.Flag) {
				v[i].SetUint64(uint64(h))
			} else {
				v[i].SetInt64(h)
			}
			continue
		}

		k := t.RecordKey(h, col)
		data, err := txn.Get(k)
		if terror.ErrorEqual(err, kv.ErrNotExist) && !mysql.HasNotNullFlag(col.Flag) {
			continue
		} else if err != nil {
			return nil, errors.Trace(err)
		}

		v[i], err = tablecodec.DecodeColumnValue(data, &col.FieldType)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	return v, nil
}

// Row implements table.Table Row interface.
func (t *Table) Row(ctx context.Context, h int64) ([]types.Datum, error) {
	// TODO: we only interested in mentioned cols
	r, err := t.RowWithCols(ctx, h, t.Cols())
	if err != nil {
		return nil, errors.Trace(err)
	}
	return r, nil
}

// LockRow implements table.Table LockRow interface.
func (t *Table) LockRow(ctx context.Context, h int64, forRead bool) error {
	txn, err := ctx.GetTxn(false)
	if err != nil {
		return errors.Trace(err)
	}
	// Get row lock key
	lockKey := t.RecordKey(h, nil)
	if forRead {
		err = txn.LockKeys(lockKey)
	} else {
		// set row lock key to current txn
		err = txn.Set(lockKey, []byte(txn.String()))
	}
	return errors.Trace(err)
}

// RemoveRecord implements table.Table RemoveRecord interface.
func (t *Table) RemoveRecord(ctx context.Context, h int64, r []types.Datum) error {
	err := t.removeRowData(ctx, h)
	if err != nil {
		return errors.Trace(err)
	}

	err = t.removeRowIndices(ctx, h, r)
	if err != nil {
		return errors.Trace(err)
	}

	return nil
}

func (t *Table) removeRowData(ctx context.Context, h int64) error {
	if err := t.LockRow(ctx, h, false); err != nil {
		return errors.Trace(err)
	}
	txn, err := ctx.GetTxn(false)
	if err != nil {
		return errors.Trace(err)
	}
	// Remove row's colume one by one
	for _, col := range t.Columns {
		k := t.RecordKey(h, col)
		err = txn.Delete([]byte(k))
		if err != nil {
			if col.State != model.StatePublic && terror.ErrorEqual(err, kv.ErrNotExist) {
				// If the column is not in public state, we may have not added the column,
				// or already deleted the column, so skip ErrNotExist error.
				continue
			}

			return errors.Trace(err)
		}
	}
	// Remove row lock
	err = txn.Delete([]byte(t.RecordKey(h, nil)))
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

// removeRowAllIndex removes all the indices of a row.
func (t *Table) removeRowIndices(ctx context.Context, h int64, rec []types.Datum) error {
	for _, v := range t.indices {
		vals, err := v.FetchValues(rec)
		if vals == nil {
			// TODO: check this
			continue
		}
		txn, err := ctx.GetTxn(false)
		if err != nil {
			return errors.Trace(err)
		}
		if err = v.Delete(txn, vals, h); err != nil {
			if v.Meta().State != model.StatePublic && terror.ErrorEqual(err, kv.ErrNotExist) {
				// If the index is not in public state, we may have not created the index,
				// or already deleted the index, so skip ErrNotExist error.
				continue
			}

			return errors.Trace(err)
		}
	}
	return nil
}

// RemoveRowIndex implements table.Table RemoveRowIndex interface.
func (t *Table) removeRowIndex(rm kv.RetrieverMutator, h int64, vals []types.Datum, idx table.Index) error {
	if err := idx.Delete(rm, vals, h); err != nil {
		return errors.Trace(err)
	}
	return nil
}

// BuildIndexForRow implements table.Table BuildIndexForRow interface.
func (t *Table) buildIndexForRow(rm kv.RetrieverMutator, h int64, vals []types.Datum, idx table.Index) error {
	if idx.Meta().State == model.StateDeleteOnly || idx.Meta().State == model.StateDeleteReorganization {
		// If the index is in delete only or write reorganization state, we can not add index.
		return nil
	}

	if err := idx.Create(rm, vals, h); err != nil {
		return errors.Trace(err)
	}
	return nil
}

// IterRecords implements table.Table IterRecords interface.
func (t *Table) IterRecords(ctx context.Context, startKey kv.Key, cols []*table.Column,
	fn table.RecordIterFunc) error {
	txn, err := ctx.GetTxn(false)
	if err != nil {
		return errors.Trace(err)
	}
	it, err := txn.Seek(startKey)
	if err != nil {
		return errors.Trace(err)
	}
	defer it.Close()

	if !it.Valid() {
		return nil
	}

	log.Debugf("startKey:%q, key:%q, value:%q", startKey, it.Key(), it.Value())

	colMap := make(map[int64]*types.FieldType)
	for _, col := range cols {
		colMap[col.ID] = &col.FieldType
	}
	prefix := t.RecordPrefix()
	for it.Valid() && it.Key().HasPrefix(prefix) {
		// first kv pair is row lock information.
		// TODO: check valid lock
		// get row handle
		handle, err := tablecodec.DecodeRowKey(it.Key())
		if err != nil {
			return errors.Trace(err)
		}
		rowMap, err := tablecodec.DecodeRow(it.Value(), colMap)
		if err != nil {
			return errors.Trace(err)
		}
		data := make([]types.Datum, 0, len(cols))
		for _, col := range cols {
			data = append(data, rowMap[col.ID])
		}
		more, err := fn(handle, data, cols)
		if !more || err != nil {
			return errors.Trace(err)
		}

		rk := t.RecordKey(handle, nil)
		err = kv.NextUntil(it, util.RowKeyPrefixFilter(rk))
		if err != nil {
			return errors.Trace(err)
		}
	}

	return nil
}

// AllocAutoID implements table.Table AllocAutoID interface.
func (t *Table) AllocAutoID() (int64, error) {
	return t.alloc.Alloc(t.ID)
}

// RebaseAutoID implements table.Table RebaseAutoID interface.
func (t *Table) RebaseAutoID(newBase int64, isSetStep bool) error {
	return t.alloc.Rebase(t.ID, newBase, isSetStep)
}

// Seek implements table.Table Seek interface.
func (t *Table) Seek(ctx context.Context, h int64) (int64, bool, error) {
	seekKey := tablecodec.EncodeColumnKey(t.ID, h, 0)
	txn, err := ctx.GetTxn(false)
	if err != nil {
		return 0, false, errors.Trace(err)
	}
	iter, err := txn.Seek(seekKey)
	if !iter.Valid() || !iter.Key().HasPrefix(t.RecordPrefix()) {
		// No more records in the table, skip to the end.
		return 0, false, nil
	}
	handle, err := tablecodec.DecodeRowKey(iter.Key())
	if err != nil {
		return 0, false, errors.Trace(err)
	}
	return handle, true, nil
}

var (
	recordPrefixSep = []byte("_r")
)

// SetColValue implements table.Table SetColValue interface.
func SetColValue(rm kv.RetrieverMutator, key []byte, data types.Datum) error {
	v, err := tablecodec.EncodeValue(data)
	if err != nil {
		return errors.Trace(err)
	}
	if err := rm.Set(key, v); err != nil {
		return errors.Trace(err)
	}
	return nil
}

// FindIndexByColName implements table.Table FindIndexByColName interface.
func FindIndexByColName(t table.Table, name string) table.Index {
	for _, idx := range t.Indices() {
		// only public index can be read.
		if idx.Meta().State != model.StatePublic {
			continue
		}

		if len(idx.Meta().Columns) == 1 && strings.EqualFold(idx.Meta().Columns[0].Name.L, name) {
			return idx
		}
	}
	return nil
}

func init() {
	table.TableFromMeta = TableFromMeta
	table.MockTableFromMeta = MockTableFromMeta
}
