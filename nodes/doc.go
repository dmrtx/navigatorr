// Package nodes is the build registry of Navigatorr: the record of the jobs
// other tools run on machines (builds), the per-node "builds paused" switch and
// the HTTP API that serves them (/nodes/v1, described by an OpenAPI document so
// the MCP reaches it as a generic service).
//
// A build is a pure record: register (create), heartbeat (progress and output
// lines), finish or cancel, expiry to lost when the heartbeat lapses, history
// with pruning, and an in-memory output tail. The registry lives in its own
// SQLite database (nodes.db by default), never in the main one.
//
// Invariants. They are tested in invariants_test.go.
//
//	N1  A running lease is decided only by the node's worker. Nodes never holds,
//	    admits, freezes or queues anything: a record, running or not, changes no
//	    transcode pause, dispatch, requeue or hold. The only refusal Nodes makes
//	    is the operator's builds pause, which the worker relies on.
//	N2  The transcode engine (package action and transcode) never imports
//	    nodes. What it needs of the records (a node's running records for
//	    display, the builds pause) it reads through an interface that main
//	    wires at startup. Nodes in turn imports neither action, transcode nor
//	    maintenanceui; it learns which targets are nodes, and what the
//	    transcode engine reports of a node, through the Topology and
//	    TranscodeView interfaces. internal/importguard enforces both sides.
//	N3  Nodes being down never stops a lease. The worker fails open when the
//	    server cannot be reached, and the server does nothing for a lease.
//	N4  The legacy routes /api/maintenance/builds* keep their request and
//	    response shapes, status codes and auth. They are thin aliases onto
//	    this package.
//	N5  A cancel through /nodes/v1 and a cancel through the legacy route are
//	    the same operation (finish with status cancelled): the next heartbeat
//	    the node worker forwards for that build is answered "build already
//	    ended" with the cancelled record, which is how the build tool learns it
//	    must stop.
package nodes
