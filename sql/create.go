// Copyright 2015 The Cockroach Authors.
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
//
// Author: Peter Mattis (peter@cockroachlabs.com)

package sql

import (
	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/security"
	"github.com/cockroachdb/cockroach/sql/parser"
	"github.com/cockroachdb/cockroach/sql/privilege"
	"github.com/cockroachdb/cockroach/util"
)

type createDatabaseNode struct {
	p *planner
	n *parser.CreateDatabase
}

// CreateDatabase creates a database.
// Privileges: security.RootUser user.
//   Notes: postgres requires superuser or "CREATEDB".
//          mysql uses the mysqladmin command.
func (p *planner) CreateDatabase(n *parser.CreateDatabase) (planNode, error) {
	if n.Name == "" {
		return nil, errEmptyDatabaseName
	}

	if p.session.User != security.RootUser {
		return nil, util.Errorf("only %s is allowed to create databases", security.RootUser)
	}

	return &createDatabaseNode{p: p, n: n}, nil
}

func (n *createDatabaseNode) Start() *roachpb.Error {
	desc := makeDatabaseDesc(n.n)

	created, err := n.p.createDescriptor(databaseKey{string(n.n.Name)}, &desc, n.n.IfNotExists)
	if err != nil {
		return roachpb.NewError(err)
	}
	if created {
		// Log Create Database event.
		if err := MakeEventLogger(n.p.leaseMgr).InsertEventRecord(n.p.txn,
			EventLogCreateDatabase,
			int32(desc.ID),
			int32(n.p.evalCtx.NodeID),
			struct {
				DatabaseName string
				Statement    string
				User         string
			}{n.n.Name.String(), n.n.String(), n.p.session.User},
		); err != nil {
			return roachpb.NewError(err)
		}
	}
	return nil
}

func (n *createDatabaseNode) Next() bool                   { return false }
func (n *createDatabaseNode) Columns() []ResultColumn      { return make([]ResultColumn, 0) }
func (n *createDatabaseNode) Ordering() orderingInfo       { return orderingInfo{} }
func (n *createDatabaseNode) Values() parser.DTuple        { return parser.DTuple{} }
func (n *createDatabaseNode) DebugValues() debugValues     { return debugValues{} }
func (n *createDatabaseNode) PErr() *roachpb.Error         { return nil }
func (n *createDatabaseNode) SetLimitHint(_ int64, _ bool) {}
func (n *createDatabaseNode) MarkDebug(mode explainMode)   {}
func (n *createDatabaseNode) ExplainPlan(v bool) (string, string, []planNode) {
	return "create database", "", nil
}

type createIndexNode struct {
	p         *planner
	n         *parser.CreateIndex
	tableDesc *TableDescriptor
}

// CreateIndex creates an index.
// Privileges: CREATE on table.
//   notes: postgres requires CREATE on the table.
//          mysql requires INDEX on the table.
func (p *planner) CreateIndex(n *parser.CreateIndex) (planNode, error) {
	tableDesc, err := p.getTableDesc(n.Table)
	if err != nil {
		return nil, err
	}
	if tableDesc == nil {
		return nil, tableDoesNotExistError(n.Table.String())
	}

	if err := p.checkPrivilege(tableDesc, privilege.CREATE); err != nil {
		return nil, err
	}

	return &createIndexNode{p: p, tableDesc: tableDesc, n: n}, nil
}

func (n *createIndexNode) Start() *roachpb.Error {
	status, i, err := n.tableDesc.FindIndexByName(string(n.n.Name))
	if err == nil {
		if status == DescriptorIncomplete {
			switch n.tableDesc.Mutations[i].Direction {
			case DescriptorMutation_DROP:
				return roachpb.NewErrorf("index %q being dropped, try again later", string(n.n.Name))

			case DescriptorMutation_ADD:
				// Noop, will fail in AllocateIDs below.
			}
		}
		if n.n.IfNotExists {
			return nil
		}
	}

	indexDesc := IndexDescriptor{
		Name:             string(n.n.Name),
		Unique:           n.n.Unique,
		StoreColumnNames: n.n.Storing,
	}
	if err := indexDesc.fillColumns(n.n.Columns); err != nil {
		return roachpb.NewError(err)
	}

	n.tableDesc.addIndexMutation(indexDesc, DescriptorMutation_ADD)
	mutationID, err := n.tableDesc.finalizeMutation()
	if err != nil {
		return roachpb.NewError(err)
	}
	if err := n.tableDesc.AllocateIDs(); err != nil {
		return roachpb.NewError(err)
	}

	if err := n.p.txn.Put(MakeDescMetadataKey(n.tableDesc.GetID()), wrapDescriptor(n.tableDesc)); err != nil {
		return roachpb.NewError(err)
	}
	n.p.notifySchemaChange(n.tableDesc.ID, mutationID)

	return nil
}

func (n *createIndexNode) Next() bool                   { return false }
func (n *createIndexNode) Columns() []ResultColumn      { return make([]ResultColumn, 0) }
func (n *createIndexNode) Ordering() orderingInfo       { return orderingInfo{} }
func (n *createIndexNode) Values() parser.DTuple        { return parser.DTuple{} }
func (n *createIndexNode) DebugValues() debugValues     { return debugValues{} }
func (n *createIndexNode) PErr() *roachpb.Error         { return nil }
func (n *createIndexNode) SetLimitHint(_ int64, _ bool) {}
func (n *createIndexNode) MarkDebug(mode explainMode)   {}
func (n *createIndexNode) ExplainPlan(v bool) (string, string, []planNode) {
	return "create index", "", nil
}

type createTableNode struct {
	p      *planner
	n      *parser.CreateTable
	dbDesc *DatabaseDescriptor
}

// CreateTable creates a table.
// Privileges: CREATE on database.
//   Notes: postgres/mysql require CREATE on database.
func (p *planner) CreateTable(n *parser.CreateTable) (planNode, error) {
	if err := n.Table.NormalizeTableName(p.session.Database); err != nil {
		return nil, err
	}

	dbDesc, err := p.getDatabaseDesc(n.Table.Database())
	if err != nil {
		return nil, err
	}
	if dbDesc == nil {
		return nil, databaseDoesNotExistError(n.Table.Database())
	}

	if err := p.checkPrivilege(dbDesc, privilege.CREATE); err != nil {
		return nil, err
	}

	return &createTableNode{p: p, n: n, dbDesc: dbDesc}, nil
}

func (n *createTableNode) Start() *roachpb.Error {
	desc, err := makeTableDesc(n.n, n.dbDesc.ID)
	if err != nil {
		return roachpb.NewError(err)
	}
	// Inherit permissions from the database descriptor.
	desc.Privileges = n.dbDesc.GetPrivileges()

	if len(desc.PrimaryIndex.ColumnNames) == 0 {
		// Ensure a Primary Key exists.
		s := "unique_rowid()"
		col := ColumnDescriptor{
			Name: "rowid",
			Type: ColumnType{
				Kind: ColumnType_INT,
			},
			DefaultExpr: &s,
			Hidden:      true,
			Nullable:    false,
		}
		desc.AddColumn(col)
		idx := IndexDescriptor{
			Unique:           true,
			ColumnNames:      []string{col.Name},
			ColumnDirections: []IndexDescriptor_Direction{IndexDescriptor_ASC},
		}
		if err := desc.AddIndex(idx, true); err != nil {
			return roachpb.NewError(err)
		}
	}

	if err := desc.AllocateIDs(); err != nil {
		return roachpb.NewError(err)
	}

	created, err := n.p.createDescriptor(tableKey{n.dbDesc.ID, n.n.Table.Table()}, &desc, n.n.IfNotExists)
	if err != nil {
		return roachpb.NewError(err)
	}

	if created {
		// Log Create Table event.
		if err := MakeEventLogger(n.p.leaseMgr).InsertEventRecord(n.p.txn,
			EventLogCreateTable,
			int32(desc.ID),
			int32(n.p.evalCtx.NodeID),
			struct {
				TableName string
				Statement string
				User      string
			}{n.n.Table.String(), n.n.String(), n.p.session.User},
		); err != nil {
			return roachpb.NewError(err)
		}
	}

	return nil
}

func (n *createTableNode) Next() bool                   { return false }
func (n *createTableNode) Columns() []ResultColumn      { return make([]ResultColumn, 0) }
func (n *createTableNode) Ordering() orderingInfo       { return orderingInfo{} }
func (n *createTableNode) Values() parser.DTuple        { return parser.DTuple{} }
func (n *createTableNode) DebugValues() debugValues     { return debugValues{} }
func (n *createTableNode) PErr() *roachpb.Error         { return nil }
func (n *createTableNode) SetLimitHint(_ int64, _ bool) {}
func (n *createTableNode) MarkDebug(mode explainMode)   {}
func (n *createTableNode) ExplainPlan(v bool) (string, string, []planNode) {
	return "create table", "", nil
}
