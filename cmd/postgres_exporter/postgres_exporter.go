// Copyright 2021 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blang/semver"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	webflag "github.com/prometheus/exporter-toolkit/web/kingpinflag"
	"gopkg.in/alecthomas/kingpin.v2"
	"gopkg.in/yaml.v2"
)

var (
	listenAddress          = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry.").Default(":9187").Envar("PG_EXPORTER_WEB_LISTEN_ADDRESS").String()
	webConfig              = webflag.AddFlags(kingpin.CommandLine)
	metricPath             = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").Envar("PG_EXPORTER_WEB_TELEMETRY_PATH").String()
	disableDefaultMetrics  = kingpin.Flag("disable-default-metrics", "Do not include default metrics.").Default("false").Envar("PG_EXPORTER_DISABLE_DEFAULT_METRICS").Bool()
	disableSettingsMetrics = kingpin.Flag("disable-settings-metrics", "Do not include pg_settings metrics.").Default("false").Envar("PG_EXPORTER_DISABLE_SETTINGS_METRICS").Bool()
	autoDiscoverDatabases  = kingpin.Flag("auto-discover-databases", "Whether to discover the databases on a server dynamically.").Default("false").Envar("PG_EXPORTER_AUTO_DISCOVER_DATABASES").Bool()
	queriesPath            = kingpin.Flag("extend.query-path", "Path to custom queries to run.").Default("").Envar("PG_EXPORTER_EXTEND_QUERY_PATH").String()
	onlyDumpMaps           = kingpin.Flag("dumpmaps", "Do not run, simply dump the maps.").Bool()
	constantLabelsList     = kingpin.Flag("constantLabels", "A list of label=value separated by comma(,).").Default("").Envar("PG_EXPORTER_CONSTANT_LABELS").String()
	excludeDatabases       = kingpin.Flag("exclude-databases", "A list of databases to remove when autoDiscoverDatabases is enabled").Default("").Envar("PG_EXPORTER_EXCLUDE_DATABASES").String()
	includeDatabases       = kingpin.Flag("include-databases", "A list of databases to include when autoDiscoverDatabases is enabled").Default("").Envar("PG_EXPORTER_INCLUDE_DATABASES").String()
	metricPrefix           = kingpin.Flag("metric-prefix", "A metric prefix can be used to have non-default (not \"pg\") prefixes for each of the metrics").Default("pg").Envar("PG_EXPORTER_METRIC_PREFIX").String()
	logger                 = log.NewNopLogger()
)

// Metric name parts.
const (
	// Namespace for all metrics.
	namespace = "pg"
	// Subsystems.
	exporter = "exporter"
	// The name of the exporter.
	exporterName = "postgres_exporter"
	// Metric label used for static string data thats handy to send to Prometheus
	// e.g. version
	staticLabelName = "static"
	// Metric label used for server identification.
	serverLabelName = "server"
)

// ColumnUsage should be one of several enum values which describe how a
// queried row is to be converted to a Prometheus metric.
type ColumnUsage int

// nolint: golint
const (
	DISCARD      ColumnUsage = iota // Ignore this column
	LABEL        ColumnUsage = iota // Use this column as a label
	COUNTER      ColumnUsage = iota // Use this column as a counter
	GAUGE        ColumnUsage = iota // Use this column as a gauge
	MAPPEDMETRIC ColumnUsage = iota // Use this column with the supplied mapping of text values
	DURATION     ColumnUsage = iota // This column should be interpreted as a text duration (and converted to milliseconds)
	HISTOGRAM    ColumnUsage = iota // Use this column as a histogram
)

// UnmarshalYAML implements the yaml.Unmarshaller interface.
func (cu *ColumnUsage) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var value string
	if err := unmarshal(&value); err != nil {
		return err
	}

	columnUsage, err := stringToColumnUsage(value)
	if err != nil {
		return err
	}

	*cu = columnUsage
	return nil
}

// MappingOptions is a copy of ColumnMapping used only for parsing
type MappingOptions struct {
	Usage             string             `yaml:"usage"`
	Description       string             `yaml:"description"`
	Mapping           map[string]float64 `yaml:"metric_mapping"` // Optional column mapping for MAPPEDMETRIC
	SupportedVersions semver.Range       `yaml:"pg_version"`     // Semantic version ranges which are supported. Unsupported columns are not queried (internally converted to DISCARD).
}

// nolint: golint
type Mapping map[string]MappingOptions

// nolint: golint
type UserQuery struct {
	Query        string    `yaml:"query"`
	Metrics      []Mapping `yaml:"metrics"`
	Master       bool      `yaml:"master"`        // Querying only for master database
	CacheSeconds uint64    `yaml:"cache_seconds"` // Number of seconds to cache the namespace result metrics for.
	RunOnServer  string    `yaml:"runonserver"`   // Querying to run on which server version
}

// nolint: golint
type UserQueries map[string]UserQuery

// Regex used to get the "short-version" from the postgres version field.
var versionRegex = regexp.MustCompile(`^\w+ ((\d+)(\.\d+)?(\.\d+)?)`)
var lowestSupportedVersion = semver.MustParse("9.1.0")

// Parses the version of postgres into the short version string we can use to
// match behaviors.
func parseVersion(versionString string) (semver.Version, error) {
	submatches := versionRegex.FindStringSubmatch(versionString)
	if len(submatches) > 1 {
		return semver.ParseTolerant(submatches[1])
	}
	return semver.Version{},
		errors.New(fmt.Sprintln("Could not find a postgres version in string:", versionString))
}

// ColumnMapping is the user-friendly representation of a prometheus descriptor map
type ColumnMapping struct {
	usage             ColumnUsage        `yaml:"usage"`
	description       string             `yaml:"description"`
	mapping           map[string]float64 `yaml:"metric_mapping"` // Optional column mapping for MAPPEDMETRIC
	supportedVersions semver.Range       `yaml:"pg_version"`     // Semantic version ranges which are supported. Unsupported columns are not queried (internally converted to DISCARD).
}

// UnmarshalYAML implements yaml.Unmarshaller
func (cm *ColumnMapping) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type plain ColumnMapping
	return unmarshal((*plain)(cm))
}

// intermediateMetricMap holds the partially loaded metric map parsing.
// This is mainly so we can parse cacheSeconds around.
type intermediateMetricMap struct {
	columnMappings map[string]ColumnMapping
	master         bool
	cacheSeconds   uint64
}

// MetricMapNamespace groups metric maps under a shared set of labels.
type MetricMapNamespace struct {
	labels         []string             // Label names for this namespace
	columnMappings map[string]MetricMap // Column mappings in this namespace
	master         bool                 // Call query only for master database
	cacheSeconds   uint64               // Number of seconds this metric namespace can be cached. 0 disables.
}

// MetricMap stores the prometheus metric description which a given column will
// be mapped to by the collector
type MetricMap struct {
	discard    bool                              // Should metric be discarded during mapping?
	histogram  bool                              // Should metric be treated as a histogram?
	vtype      prometheus.ValueType              // Prometheus valuetype
	desc       *prometheus.Desc                  // Prometheus descriptor
	conversion func(interface{}) (float64, bool) // Conversion function to turn PG result into float64
}

// ErrorConnectToServer is a connection to PgSQL server error
type ErrorConnectToServer struct {
	Msg string
}

// Error returns error
func (e *ErrorConnectToServer) Error() string {
	return e.Msg
}

// TODO: revisit this with the semver system
func dumpMaps() {
	// TODO: make this function part of the exporter
	for name, cmap := range builtinMetricMaps {
		query, ok := queryOverrides[name]
		if !ok {
			fmt.Println(name)
		} else {
			for _, queryOverride := range query {
				fmt.Println(name, queryOverride.versionRange, queryOverride.query)
			}
		}

		for column, details := range cmap.columnMappings {
			fmt.Printf("  %-40s %v\n", column, details)
		}
		fmt.Println()
	}
}

var builtinMetricMaps = map[string]intermediateMetricMap{
	"pg_stat_bgwriter": {
		map[string]ColumnMapping{
			"checkpoints_timed":     {COUNTER, "Number of scheduled checkpoints that have been performed", nil, nil},
			"checkpoints_req":       {COUNTER, "Number of requested checkpoints that have been performed", nil, nil},
			"checkpoint_write_time": {COUNTER, "Total amount of time that has been spent in the portion of checkpoint processing where files are written to disk, in milliseconds", nil, nil},
			"checkpoint_sync_time":  {COUNTER, "Total amount of time that has been spent in the portion of checkpoint processing where files are synchronized to disk, in milliseconds", nil, nil},
			"buffers_checkpoint":    {COUNTER, "Number of buffers written during checkpoints", nil, nil},
			"buffers_clean":         {COUNTER, "Number of buffers written by the background writer", nil, nil},
			"maxwritten_clean":      {COUNTER, "Number of times the background writer stopped a cleaning scan because it had written too many buffers", nil, nil},
			"buffers_backend":       {COUNTER, "Number of buffers written directly by a backend", nil, nil},
			"buffers_backend_fsync": {COUNTER, "Number of times a backend had to execute its own fsync call (normally the background writer handles those even when the backend does its own write)", nil, nil},
			"buffers_alloc":         {COUNTER, "Number of buffers allocated", nil, nil},
			"stats_reset":           {COUNTER, "Time at which these statistics were last reset", nil, nil},
		},
		true,
		0,
	},
	"pg_stat_database": {
		map[string]ColumnMapping{
			"datid":          {LABEL, "OID of a database", nil, nil},
			"datname":        {LABEL, "Name of this database", nil, nil},
			"numbackends":    {GAUGE, "Number of backends currently connected to this database. This is the only column in this view that returns a value reflecting current state; all other columns return the accumulated values since the last reset.", nil, nil},
			"xact_commit":    {COUNTER, "Number of transactions in this database that have been committed", nil, nil},
			"xact_rollback":  {COUNTER, "Number of transactions in this database that have been rolled back", nil, nil},
			"blks_read":      {COUNTER, "Number of disk blocks read in this database", nil, nil},
			"blks_hit":       {COUNTER, "Number of times disk blocks were found already in the buffer cache, so that a read was not necessary (this only includes hits in the PostgreSQL buffer cache, not the operating system's file system cache)", nil, nil},
			"tup_returned":   {COUNTER, "Number of rows returned by queries in this database", nil, nil},
			"tup_fetched":    {COUNTER, "Number of rows fetched by queries in this database", nil, nil},
			"tup_inserted":   {COUNTER, "Number of rows inserted by queries in this database", nil, nil},
			"tup_updated":    {COUNTER, "Number of rows updated by queries in this database", nil, nil},
			"tup_deleted":    {COUNTER, "Number of rows deleted by queries in this database", nil, nil},
			"conflicts":      {COUNTER, "Number of queries canceled due to conflicts with recovery in this database. (Conflicts occur only on standby servers; see pg_stat_database_conflicts for details.)", nil, nil},
			"temp_files":     {COUNTER, "Number of temporary files created by queries in this database. All temporary files are counted, regardless of why the temporary file was created (e.g., sorting or hashing), and regardless of the log_temp_files setting.", nil, nil},
			"temp_bytes":     {COUNTER, "Total amount of data written to temporary files by queries in this database. All temporary files are counted, regardless of why the temporary file was created, and regardless of the log_temp_files setting.", nil, nil},
			"deadlocks":      {COUNTER, "Number of deadlocks detected in this database", nil, nil},
			"blk_read_time":  {COUNTER, "Time spent reading data file blocks by backends in this database, in milliseconds", nil, nil},
			"blk_write_time": {COUNTER, "Time spent writing data file blocks by backends in this database, in milliseconds", nil, nil},
			"stats_reset":    {COUNTER, "Time at which these statistics were last reset", nil, nil},
		},
		true,
		0,
	},
	"pg_stat_database_conflicts": {
		map[string]ColumnMapping{
			"datid":            {LABEL, "OID of a database", nil, nil},
			"datname":          {LABEL, "Name of this database", nil, nil},
			"confl_tablespace": {COUNTER, "Number of queries in this database that have been canceled due to dropped tablespaces", nil, nil},
			"confl_lock":       {COUNTER, "Number of queries in this database that have been canceled due to lock timeouts", nil, nil},
			"confl_snapshot":   {COUNTER, "Number of queries in this database that have been canceled due to old snapshots", nil, nil},
			"confl_bufferpin":  {COUNTER, "Number of queries in this database that have been canceled due to pinned buffers", nil, nil},
			"confl_deadlock":   {COUNTER, "Number of queries in this database that have been canceled due to deadlocks", nil, nil},
		},
		true,
		0,
	},
	"pg_locks": {
		map[string]ColumnMapping{
			"datname": {LABEL, "Name of this database", nil, nil},
			"mode":    {LABEL, "Type of Lock", nil, nil},
			"count":   {GAUGE, "Number of locks", nil, nil},
		},
		true,
		0,
	},
	"pg_stat_replication": {
		map[string]ColumnMapping{
			"procpid":          {DISCARD, "Process ID of a WAL sender process", nil, semver.MustParseRange("<9.2.0")},
			"pid":              {DISCARD, "Process ID of a WAL sender process", nil, semver.MustParseRange(">=9.2.0")},
			"usesysid":         {DISCARD, "OID of the user logged into this WAL sender process", nil, nil},
			"usename":          {DISCARD, "Name of the user logged into this WAL sender process", nil, nil},
			"application_name": {LABEL, "Name of the application that is connected to this WAL sender", nil, nil},
			"client_addr":      {LABEL, "IP address of the client connected to this WAL sender. If this field is null, it indicates that the client is connected via a Unix socket on the server machine.", nil, nil},
			"client_hostname":  {DISCARD, "Host name of the connected client, as reported by a reverse DNS lookup of client_addr. This field will only be non-null for IP connections, and only when log_hostname is enabled.", nil, nil},
			"client_port":      {DISCARD, "TCP port number that the client is using for communication with this WAL sender, or -1 if a Unix socket is used", nil, nil},
			"backend_start": {DISCARD, "with time zone	Time when this process was started, i.e., when the client connected to this WAL sender", nil, nil},
			"backend_xmin":             {DISCARD, "The current backend's xmin horizon.", nil, nil},
			"state":                    {LABEL, "Current WAL sender state", nil, nil},
			"sent_location":            {DISCARD, "Last transaction log position sent on this connection", nil, semver.MustParseRange("<10.0.0")},
			"write_location":           {DISCARD, "Last transaction log position written to disk by this standby server", nil, semver.MustParseRange("<10.0.0")},
			"flush_location":           {DISCARD, "Last transaction log position flushed to disk by this standby server", nil, semver.MustParseRange("<10.0.0")},
			"replay_location":          {DISCARD, "Last transaction log position replayed into the database on this standby server", nil, semver.MustParseRange("<10.0.0")},
			"sent_lsn":                 {DISCARD, "Last transaction log position sent on this connection", nil, semver.MustParseRange(">=10.0.0")},
			"write_lsn":                {DISCARD, "Last transaction log position written to disk by this standby server", nil, semver.MustParseRange(">=10.0.0")},
			"flush_lsn":                {DISCARD, "Last transaction log position flushed to disk by this standby server", nil, semver.MustParseRange(">=10.0.0")},
			"replay_lsn":               {DISCARD, "Last transaction log position replayed into the database on this standby server", nil, semver.MustParseRange(">=10.0.0")},
			"sync_priority":            {DISCARD, "Priority of this standby server for being chosen as the synchronous standby", nil, nil},
			"sync_state":               {DISCARD, "Synchronous state of this standby server", nil, nil},
			"slot_name":                {LABEL, "A unique, cluster-wide identifier for the replication slot", nil, semver.MustParseRange(">=9.2.0")},
			"plugin":                   {DISCARD, "The base name of the shared object containing the output plugin this logical slot is using, or null for physical slots", nil, nil},
			"slot_type":                {DISCARD, "The slot type - physical or logical", nil, nil},
			"datoid":                   {DISCARD, "The OID of the database this slot is associated with, or null. Only logical slots have an associated database", nil, nil},
			"database":                 {DISCARD, "The name of the database this slot is associated with, or null. Only logical slots have an associated database", nil, nil},
			"active":                   {DISCARD, "True if this slot is currently actively being used", nil, nil},
			"active_pid":               {DISCARD, "Process ID of a WAL sender process", nil, nil},
			"xmin":                     {DISCARD, "The oldest transaction that this slot needs the database to retain. VACUUM cannot remove tuples deleted by any later transaction", nil, nil},
			"catalog_xmin":             {DISCARD, "The oldest transaction affecting the system catalogs that this slot needs the database to retain. VACUUM cannot remove catalog tuples deleted by any later transaction", nil, nil},
			"restart_lsn":              {DISCARD, "The address (LSN) of oldest WAL which still might be required by the consumer of this slot and thus won't be automatically removed during checkpoints", nil, nil},
			"pg_current_xlog_location": {DISCARD, "pg_current_xlog_location", nil, nil},
			"pg_current_wal_lsn":       {DISCARD, "pg_current_xlog_location", nil, semver.MustParseRange(">=10.0.0")},
			"pg_current_wal_lsn_bytes": {GAUGE, "WAL position in bytes", nil, semver.MustParseRange(">=10.0.0")},
			"pg_xlog_location_diff":    {GAUGE, "Lag in bytes between master and slave", nil, semver.MustParseRange(">=9.2.0 <10.0.0")},
			"pg_wal_lsn_diff":          {GAUGE, "Lag in bytes between master and slave", nil, semver.MustParseRange(">=10.0.0")},
			"confirmed_flush_lsn":      {DISCARD, "LSN position a consumer of a slot has confirmed flushing the data received", nil, nil},
			"write_lag":                {DISCARD, "Time elapsed between flushing recent WAL locally and receiving notification that this standby server has written it (but not yet flushed it or applied it). This can be used to gauge the delay that synchronous_commit level remote_write incurred while committing if this server was configured as a synchronous standby.", nil, semver.MustParseRange(">=10.0.0")},
			"flush_lag":                {DISCARD, "Time elapsed between flushing recent WAL locally and receiving notification that this standby server has written and flushed it (but not yet applied it). This can be used to gauge the delay that synchronous_commit level remote_flush incurred while committing if this server was configured as a synchronous standby.", nil, semver.MustParseRange(">=10.0.0")},
			"replay_lag":               {DISCARD, "Time elapsed between flushing recent WAL locally and receiving notification that this standby server has written, flushed and applied it. This can be used to gauge the delay that synchronous_commit level remote_apply incurred while committing if this server was configured as a synchronous standby.", nil, semver.MustParseRange(">=10.0.0")},
		},
		true,
		0,
	},
	"pg_replication_slots": {
		map[string]ColumnMapping{
			"slot_name":       {LABEL, "Name of the replication slot", nil, nil},
			"database":        {LABEL, "Name of the database", nil, nil},
			"active":          {GAUGE, "Flag indicating if the slot is active", nil, nil},
			"pg_wal_lsn_diff": {GAUGE, "Replication lag in bytes", nil, nil},
		},
		true,
		0,
	},
	"pg_stat_archiver": {
		map[string]ColumnMapping{
			"archived_count":     {COUNTER, "Number of WAL files that have been successfully archived", nil, nil},
			"last_archived_wal":  {DISCARD, "Name of the last WAL file successfully archived", nil, nil},
			"last_archived_time": {DISCARD, "Time of the last successful archive operation", nil, nil},
			"failed_count":       {COUNTER, "Number of failed attempts for archiving WAL files", nil, nil},
			"last_failed_wal":    {DISCARD, "Name of the WAL file of the last failed archival operation", nil, nil},
			"last_failed_time":   {DISCARD, "Time of the last failed archival operation", nil, nil},
			"stats_reset":        {DISCARD, "Time at which these statistics were last reset", nil, nil},
			"last_archive_age":   {GAUGE, "Time in seconds since last WAL segment was successfully archived", nil, nil},
		},
		true,
		0,
	},
	"pg_stat_activity": {
		map[string]ColumnMapping{
			"datname":         {LABEL, "Name of this database", nil, nil},
			"state":           {LABEL, "connection state", nil, semver.MustParseRange(">=9.2.0")},
			"count":           {GAUGE, "number of connections in this state", nil, nil},
			"max_tx_duration": {GAUGE, "max duration in seconds any active transaction has been running", nil, nil},
		},
		true,
		0,
	},
}

// OverrideQuery 's are run in-place of simple namespace look ups, and provide
// advanced functionality. But they have a tendency to postgres version specific.
// There aren't too many versions, so we simply store customized versions using
// the semver matching we do for columns.
type OverrideQuery struct {
	versionRange semver.Range
	query        string
}

// Overriding queries for namespaces above.
// TODO: validate this is a closed set in tests, and there are no overlaps
var queryOverrides = map[string][]OverrideQuery{
	"pg_locks": {
		{
			semver.MustParseRange(">0.0.0"),
			`SELECT pg_database.datname,tmp.mode,COALESCE(count,0) as count
			FROM
				(
				  VALUES ('accesssharelock'),
				         ('rowsharelock'),
				         ('rowexclusivelock'),
				         ('shareupdateexclusivelock'),
				         ('sharelock'),
				         ('sharerowexclusivelock'),
				         ('exclusivelock'),
				         ('accessexclusivelock'),
					 ('sireadlock')
				) AS tmp(mode) CROSS JOIN pg_database
			LEFT JOIN
			  (SELECT database, lower(mode) AS mode,count(*) AS count
			  FROM pg_locks WHERE database IS NOT NULL
			  GROUP BY database, lower(mode)
			) AS tmp2
			ON tmp.mode=tmp2.mode and pg_database.oid = tmp2.database ORDER BY 1`,
		},
	},

	"pg_stat_replication": {
		{
			semver.MustParseRange(">=10.0.0"),
			`
			SELECT *,
				(case pg_is_in_recovery() when 't' then null else pg_current_wal_lsn() end) AS pg_current_wal_lsn,
				(case pg_is_in_recovery() when 't' then null else pg_wal_lsn_diff(pg_current_wal_lsn(), pg_lsn('0/0'))::float end) AS pg_current_wal_lsn_bytes,
				(case pg_is_in_recovery() when 't' then null else pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn)::float end) AS pg_wal_lsn_diff
			FROM pg_stat_replication
			`,
		},
		{
			semver.MustParseRange(">=9.2.0 <10.0.0"),
			`
			SELECT *,
				(case pg_is_in_recovery() when 't' then null else pg_current_xlog_location() end) AS pg_current_xlog_location,
				(case pg_is_in_recovery() when 't' then null else pg_xlog_location_diff(pg_current_xlog_location(), replay_location)::float end) AS pg_xlog_location_diff
			FROM pg_stat_replication
			`,
		},
		{
			semver.MustParseRange("<9.2.0"),
			`
			SELECT *,
				(case pg_is_in_recovery() when 't' then null else pg_current_xlog_location() end) AS pg_current_xlog_location
			FROM pg_stat_replication
			`,
		},
	},

	"pg_replication_slots": {
		{
			semver.MustParseRange(">=9.4.0 <10.0.0"),
			`
			SELECT slot_name, database, active, pg_xlog_location_diff(pg_current_xlog_location(), restart_lsn)
			FROM pg_replication_slots
			`,
		},
		{
			semver.MustParseRange(">=10.0.0"),
			`
			SELECT slot_name, database, active, pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)
			FROM pg_replication_slots
			`,
		},
	},

	"pg_stat_archiver": {
		{
			semver.MustParseRange(">=0.0.0"),
			`
			SELECT *,
				extract(epoch from now() - last_archived_time) AS last_archive_age
			FROM pg_stat_archiver
			`,
		},
	},

	"pg_stat_activity": {
		// This query only works
		{
			semver.MustParseRange(">=9.2.0"),
			`
			SELECT
				pg_database.datname,
				tmp.state,
				COALESCE(count,0) as count,
				COALESCE(max_tx_duration,0) as max_tx_duration
			FROM
				(
				  VALUES ('active'),
				  		 ('idle'),
				  		 ('idle in transaction'),
				  		 ('idle in transaction (aborted)'),
				  		 ('fastpath function call'),
				  		 ('disabled')
				) AS tmp(state) CROSS JOIN pg_database
			LEFT JOIN
			(
				SELECT
					datname,
					state,
					count(*) AS count,
					MAX(EXTRACT(EPOCH FROM now() - xact_start))::float AS max_tx_duration
				FROM pg_stat_activity GROUP BY datname,state) AS tmp2
				ON tmp.state = tmp2.state AND pg_database.datname = tmp2.datname
			`,
		},
		{
			semver.MustParseRange("<9.2.0"),
			`
			SELECT
				datname,
				'unknown' AS state,
				COALESCE(count(*),0) AS count,
				COALESCE(MAX(EXTRACT(EPOCH FROM now() - xact_start))::float,0) AS max_tx_duration
			FROM pg_stat_activity GROUP BY datname
			`,
		},
	},
}

// Convert the query override file to the version-specific query override file
// for the exporter.
func makeQueryOverrideMap(pgVersion semver.Version, queryOverrides map[string][]OverrideQuery) map[string]string {
	resultMap := make(map[string]string)
	for name, overrideDef := range queryOverrides {
		// Find a matching semver. We make it an error to have overlapping
		// ranges at test-time, so only 1 should ever match.
		matched := false
		for _, queryDef := range overrideDef {
			if queryDef.versionRange(pgVersion) {
				resultMap[name] = queryDef.query
				matched = true
				break
			}
		}
		if !matched {
			level.Warn(logger).Log("msg", "No query matched override, disabling metric space", "name", name)
			resultMap[name] = ""
		}
	}

	return resultMap
}

func parseUserQueries(content []byte) (map[string]intermediateMetricMap, map[string]string, error) {
	var userQueries UserQueries

	err := yaml.Unmarshal(content, &userQueries)
	if err != nil {
		return nil, nil, err
	}

	// Stores the loaded map representation
	metricMaps := make(map[string]intermediateMetricMap)
	newQueryOverrides := make(map[string]string)

	for metric, specs := range userQueries {
		level.Debug(logger).Log("msg", "New user metric namespace from YAML metric", "metric", metric, "cache_seconds", specs.CacheSeconds)
		newQueryOverrides[metric] = specs.Query
		metricMap, ok := metricMaps[metric]
		if !ok {
			// Namespace for metric not found - add it.
			newMetricMap := make(map[string]ColumnMapping)
			metricMap = intermediateMetricMap{
				columnMappings: newMetricMap,
				master:         specs.Master,
				cacheSeconds:   specs.CacheSeconds,
			}
			metricMaps[metric] = metricMap
		}
		for _, metric := range specs.Metrics {
			for name, mappingOption := range metric {
				var columnMapping ColumnMapping
				tmpUsage, _ := stringToColumnUsage(mappingOption.Usage)
				columnMapping.usage = tmpUsage
				columnMapping.description = mappingOption.Description

				// TODO: we should support cu
				columnMapping.mapping = nil
				// Should we support this for users?
				columnMapping.supportedVersions = nil

				metricMap.columnMappings[name] = columnMapping
			}
		}
	}
	return metricMaps, newQueryOverrides, nil
}

// Add queries to the builtinMetricMaps and queryOverrides maps. Added queries do not
// respect version requirements, because it is assumed that the user knows
// what they are doing with their version of postgres.
//
// This function modifies metricMap and queryOverrideMap to contain the new
// queries.
// TODO: test code for all cu.
// TODO: the YAML this supports is "non-standard" - we should move away from it.
func addQueries(content []byte, pgVersion semver.Version, server *Server) error {
	metricMaps, newQueryOverrides, err := parseUserQueries(content)
	if err != nil {
		return err
	}
	// Convert the loaded metric map into exporter representation
	partialExporterMap := makeDescMap(pgVersion, server.labels, metricMaps)

	// Merge the two maps (which are now quite flatteend)
	for k, v := range partialExporterMap {
		_, found := server.metricMap[k]
		if found {
			level.Debug(logger).Log("msg", "Overriding metric from user YAML file", "metric", k)
		} else {
			level.Debug(logger).Log("msg", "Adding new metric from user YAML file", "metric", k)
		}
		server.metricMap[k] = v
	}

	// Merge the query override map
	for k, v := range newQueryOverrides {
		_, found := server.queryOverrides[k]
		if found {
			level.Debug(logger).Log("msg", "Overriding query override from user YAML file", "query_override", k)
		} else {
			level.Debug(logger).Log("msg", "Adding new query override from user YAML file", "query_override", k)
		}
		server.queryOverrides[k] = v
	}
	return nil
}

// Turn the MetricMap column mapping into a prometheus descriptor mapping.
func makeDescMap(pgVersion semver.Version, serverLabels prometheus.Labels, metricMaps map[string]intermediateMetricMap) map[string]MetricMapNamespace {
	var metricMap = make(map[string]MetricMapNamespace)

	for namespace, intermediateMappings := range metricMaps {
		thisMap := make(map[string]MetricMap)

		namespace = strings.Replace(namespace, "pg", *metricPrefix, 1)

		// Get the constant labels
		var variableLabels []string
		for columnName, columnMapping := range intermediateMappings.columnMappings {
			if columnMapping.usage == LABEL {
				variableLabels = append(variableLabels, columnName)
			}
		}

		for columnName, columnMapping := range intermediateMappings.columnMappings {
			// Check column version compatibility for the current map
			// Force to discard if not compatible.
			if columnMapping.supportedVersions != nil {
				if !columnMapping.supportedVersions(pgVersion) {
					// It's very useful to be able to see what columns are being
					// rejected.
					level.Debug(logger).Log("msg", "Column is being forced to discard due to version incompatibility", "column", columnName)
					thisMap[columnName] = MetricMap{
						discard: true,
						conversion: func(_ interface{}) (float64, bool) {
							return math.NaN(), true
						},
					}
					continue
				}
			}

			// Determine how to convert the column based on its usage.
			// nolint: dupl
			switch columnMapping.usage {
			case DISCARD, LABEL:
				thisMap[columnName] = MetricMap{
					discard: true,
					conversion: func(_ interface{}) (float64, bool) {
						return math.NaN(), true
					},
				}
			case COUNTER:
				thisMap[columnName] = MetricMap{
					vtype: prometheus.CounterValue,
					desc:  prometheus.NewDesc(fmt.Sprintf("%s_%s", namespace, columnName), columnMapping.description, variableLabels, serverLabels),
					conversion: func(in interface{}) (float64, bool) {
						return dbToFloat64(in)
					},
				}
			case GAUGE:
				thisMap[columnName] = MetricMap{
					vtype: prometheus.GaugeValue,
					desc:  prometheus.NewDesc(fmt.Sprintf("%s_%s", namespace, columnName), columnMapping.description, variableLabels, serverLabels),
					conversion: func(in interface{}) (float64, bool) {
						return dbToFloat64(in)
					},
				}
			case HISTOGRAM:
				thisMap[columnName] = MetricMap{
					histogram: true,
					vtype:     prometheus.UntypedValue,
					desc:      prometheus.NewDesc(fmt.Sprintf("%s_%s", namespace, columnName), columnMapping.description, variableLabels, serverLabels),
					conversion: func(in interface{}) (float64, bool) {
						return dbToFloat64(in)
					},
				}
				thisMap[columnName+"_bucket"] = MetricMap{
					histogram: true,
					discard:   true,
				}
				thisMap[columnName+"_sum"] = MetricMap{
					histogram: true,
					discard:   true,
				}
				thisMap[columnName+"_count"] = MetricMap{
					histogram: true,
					discard:   true,
				}
			case MAPPEDMETRIC:
				thisMap[columnName] = MetricMap{
					vtype: prometheus.GaugeValue,
					desc:  prometheus.NewDesc(fmt.Sprintf("%s_%s", namespace, columnName), columnMapping.description, variableLabels, serverLabels),
					conversion: func(in interface{}) (float64, bool) {
						text, ok := in.(string)
						if !ok {
							return math.NaN(), false
						}

						val, ok := columnMapping.mapping[text]
						if !ok {
							return math.NaN(), false
						}
						return val, true
					},
				}
			case DURATION:
				thisMap[columnName] = MetricMap{
					vtype: prometheus.GaugeValue,
					desc:  prometheus.NewDesc(fmt.Sprintf("%s_%s_milliseconds", namespace, columnName), columnMapping.description, variableLabels, serverLabels),
					conversion: func(in interface{}) (float64, bool) {
						var durationString string
						switch t := in.(type) {
						case []byte:
							durationString = string(t)
						case string:
							durationString = t
						default:
							level.Error(logger).Log("msg", "Duration conversion metric was not a string")
							return math.NaN(), false
						}

						if durationString == "-1" {
							return math.NaN(), false
						}

						d, err := time.ParseDuration(durationString)
						if err != nil {
							level.Error(logger).Log("msg", "Failed converting result to metric", "column", columnName, "in", in, "err", err)
							return math.NaN(), false
						}
						return float64(d / time.Millisecond), true
					},
				}
			}
		}

		metricMap[namespace] = MetricMapNamespace{variableLabels, thisMap, intermediateMappings.master, intermediateMappings.cacheSeconds}
	}

	return metricMap
}

// convert a string to the corresponding ColumnUsage
func stringToColumnUsage(s string) (ColumnUsage, error) {
	var u ColumnUsage
	var err error
	switch s {
	case "DISCARD":
		u = DISCARD

	case "LABEL":
		u = LABEL

	case "COUNTER":
		u = COUNTER

	case "GAUGE":
		u = GAUGE

	case "HISTOGRAM":
		u = HISTOGRAM

	case "MAPPEDMETRIC":
		u = MAPPEDMETRIC

	case "DURATION":
		u = DURATION

	default:
		err = fmt.Errorf("wrong ColumnUsage given : %s", s)
	}

	return u, err
}

// Convert database.sql types to float64s for Prometheus consumption. Null types are mapped to NaN. string and []byte
// types are mapped as NaN and !ok
func dbToFloat64(t interface{}) (float64, bool) {
	switch v := t.(type) {
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case time.Time:
		return float64(v.Unix()), true
	case []byte:
		// Try and convert to string and then parse to a float64
		strV := string(v)
		result, err := strconv.ParseFloat(strV, 64)
		if err != nil {
			level.Info(logger).Log("msg", "Could not parse []byte", "err", err)
			return math.NaN(), false
		}
		return result, true
	case string:
		result, err := strconv.ParseFloat(v, 64)
		if err != nil {
			level.Info(logger).Log("msg", "Could not parse string", "err", err)
			return math.NaN(), false
		}
		return result, true
	case bool:
		if v {
			return 1.0, true
		}
		return 0.0, true
	case nil:
		return math.NaN(), true
	default:
		return math.NaN(), false
	}
}

// Convert database.sql types to uint64 for Prometheus consumption. Null types are mapped to 0. string and []byte
// types are mapped as 0 and !ok
func dbToUint64(t interface{}) (uint64, bool) {
	switch v := t.(type) {
	case uint64:
		return v, true
	case int64:
		return uint64(v), true
	case float64:
		return uint64(v), true
	case time.Time:
		return uint64(v.Unix()), true
	case []byte:
		// Try and convert to string and then parse to a uint64
		strV := string(v)
		result, err := strconv.ParseUint(strV, 10, 64)
		if err != nil {
			level.Info(logger).Log("msg", "Could not parse []byte", "err", err)
			return 0, false
		}
		return result, true
	case string:
		result, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			level.Info(logger).Log("msg", "Could not parse string", "err", err)
			return 0, false
		}
		return result, true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case nil:
		return 0, true
	default:
		return 0, false
	}
}

// Convert database.sql to string for Prometheus labels. Null types are mapped to empty strings.
func dbToString(t interface{}) (string, bool) {
	switch v := t.(type) {
	case int64:
		return fmt.Sprintf("%v", v), true
	case float64:
		return fmt.Sprintf("%v", v), true
	case time.Time:
		return fmt.Sprintf("%v", v.Unix()), true
	case nil:
		return "", true
	case []byte:
		// Try and convert to string
		return string(v), true
	case string:
		return v, true
	case bool:
		if v {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}

func parseFingerprint(url string) (string, error) {
	dsn, err := pq.ParseURL(url)
	if err != nil {
		dsn = url
	}

	pairs := strings.Split(dsn, " ")
	kv := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		splitted := strings.SplitN(pair, "=", 2)
		if len(splitted) != 2 {
			return "", fmt.Errorf("malformed dsn %q", dsn)
		}
		kv[splitted[0]] = splitted[1]
	}

	var fingerprint string

	if host, ok := kv["host"]; ok {
		fingerprint += host
	} else {
		fingerprint += "localhost"
	}

	if port, ok := kv["port"]; ok {
		fingerprint += ":" + port
	} else {
		fingerprint += ":5432"
	}

	return fingerprint, nil
}

func loggableDSN(dsn string) string {
	pDSN, err := url.Parse(dsn)
	if err != nil {
		return "could not parse DATA_SOURCE_NAME"
	}
	// Blank user info if not nil
	if pDSN.User != nil {
		pDSN.User = url.UserPassword(pDSN.User.Username(), "PASSWORD_REMOVED")
	}

	return pDSN.String()
}

type cachedMetrics struct {
	metrics    []prometheus.Metric
	lastScrape time.Time
}

// Server describes a connection to Postgres.
// Also it contains metrics map and query overrides.
type Server struct {
	db          *sql.DB
	labels      prometheus.Labels
	master      bool
	runonserver string

	// Last version used to calculate metric map. If mismatch on scrape,
	// then maps are recalculated.
	lastMapVersion semver.Version
	// Currently active metric map
	metricMap map[string]MetricMapNamespace
	// Currently active query overrides
	queryOverrides map[string]string
	mappingMtx     sync.RWMutex
	// Currently cached metrics
	metricCache map[string]cachedMetrics
	cacheMtx    sync.Mutex
}

// ServerOpt configures a server.
type ServerOpt func(*Server)

// ServerWithLabels configures a set of labels.
func ServerWithLabels(labels prometheus.Labels) ServerOpt {
	return func(s *Server) {
		for k, v := range labels {
			s.labels[k] = v
		}
	}
}

// NewServer establishes a new connection using DSN.
func NewServer(dsn string, opts ...ServerOpt) (*Server, error) {
	fingerprint, err := parseFingerprint(dsn)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	level.Info(logger).Log("msg", "Established new database connection", "fingerprint", fingerprint)

	s := &Server{
		db:     db,
		master: false,
		labels: prometheus.Labels{
			serverLabelName: fingerprint,
		},
		metricCache: make(map[string]cachedMetrics),
	}

	for _, opt := range opts {
		opt(s)
	}

	return s, nil
}

// Close disconnects from Postgres.
func (s *Server) Close() error {
	return s.db.Close()
}

// Ping checks connection availability and possibly invalidates the connection if it fails.
func (s *Server) Ping() error {
	if err := s.db.Ping(); err != nil {
		if cerr := s.Close(); cerr != nil {
			level.Error(logger).Log("msg", "Error while closing non-pinging DB connection", "server", s, "err", cerr)
		}
		return err
	}
	return nil
}

// String returns server's fingerprint.
func (s *Server) String() string {
	return s.labels[serverLabelName]
}

// Scrape loads metrics.
func (s *Server) Scrape(ch chan<- prometheus.Metric, disableSettingsMetrics bool) error {
	s.mappingMtx.RLock()
	defer s.mappingMtx.RUnlock()

	var err error

	if !disableSettingsMetrics && s.master {
		if err = querySettings(ch, s); err != nil {
			err = fmt.Errorf("error retrieving settings: %s", err)
		}
	}

	errMap := queryNamespaceMappings(ch, s)
	if len(errMap) > 0 {
		err = fmt.Errorf("queryNamespaceMappings returned %d errors", len(errMap))
	}

	return err
}

// Servers contains a collection of servers to Postgres.
type Servers struct {
	m       sync.Mutex
	servers map[string]*Server
	opts    []ServerOpt
}

// NewServers creates a collection of servers to Postgres.
func NewServers(opts ...ServerOpt) *Servers {
	return &Servers{
		servers: make(map[string]*Server),
		opts:    opts,
	}
}

// GetServer returns established connection from a collection.
func (s *Servers) GetServer(dsn string) (*Server, error) {
	s.m.Lock()
	defer s.m.Unlock()
	var err error
	var ok bool
	errCount := 0 // start at zero because we increment before doing work
	retries := 1
	var server *Server
	for {
		if errCount++; errCount > retries {
			return nil, err
		}
		server, ok = s.servers[dsn]
		if !ok {
			server, err = NewServer(dsn, s.opts...)
			if err != nil {
				time.Sleep(time.Duration(errCount) * time.Second)
				continue
			}
			s.servers[dsn] = server
		}
		if err = server.Ping(); err != nil {
			delete(s.servers, dsn)
			time.Sleep(time.Duration(errCount) * time.Second)
			continue
		}
		break
	}
	return server, nil
}

// Close disconnects from all known servers.
func (s *Servers) Close() {
	s.m.Lock()
	defer s.m.Unlock()
	for _, server := range s.servers {
		if err := server.Close(); err != nil {
			level.Error(logger).Log("msg", "Failed to close connection", "server", server, "err", err)
		}
	}
}

// Exporter collects Postgres metrics. It implements prometheus.Collector.
type Exporter struct {
	// Holds a reference to the build in column mappings. Currently this is for testing purposes
	// only, since it just points to the global.
	builtinMetricMaps map[string]intermediateMetricMap

	disableDefaultMetrics, disableSettingsMetrics, autoDiscoverDatabases bool

	excludeDatabases []string
	includeDatabases []string
	dsn              []string
	userQueriesPath  string
	constantLabels   prometheus.Labels
	duration         prometheus.Gauge
	error            prometheus.Gauge
	psqlUp           prometheus.Gauge
	userQueriesError *prometheus.GaugeVec
	totalScrapes     prometheus.Counter

	// servers are used to allow re-using the DB connection between scrapes.
	// servers contains metrics map and query overrides.
	servers *Servers
}

// ExporterOpt configures Exporter.
type ExporterOpt func(*Exporter)

// DisableDefaultMetrics configures default metrics export.
func DisableDefaultMetrics(b bool) ExporterOpt {
	return func(e *Exporter) {
		e.disableDefaultMetrics = b
	}
}

// DisableSettingsMetrics configures pg_settings export.
func DisableSettingsMetrics(b bool) ExporterOpt {
	return func(e *Exporter) {
		e.disableSettingsMetrics = b
	}
}

// AutoDiscoverDatabases allows scraping all databases on a database server.
func AutoDiscoverDatabases(b bool) ExporterOpt {
	return func(e *Exporter) {
		e.autoDiscoverDatabases = b
	}
}

// ExcludeDatabases allows to filter out result from AutoDiscoverDatabases
func ExcludeDatabases(s string) ExporterOpt {
	return func(e *Exporter) {
		e.excludeDatabases = strings.Split(s, ",")
	}
}

// IncludeDatabases allows to filter result from AutoDiscoverDatabases
func IncludeDatabases(s string) ExporterOpt {
	return func(e *Exporter) {
		if len(s) > 0 {
			e.includeDatabases = strings.Split(s, ",")
		}
	}
}

// WithUserQueriesPath configures user's queries path.
func WithUserQueriesPath(p string) ExporterOpt {
	return func(e *Exporter) {
		e.userQueriesPath = p
	}
}

// WithConstantLabels configures constant labels.
func WithConstantLabels(s string) ExporterOpt {
	return func(e *Exporter) {
		e.constantLabels = parseConstLabels(s)
	}
}

func parseConstLabels(s string) prometheus.Labels {
	labels := make(prometheus.Labels)

	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return labels
	}

	parts := strings.Split(s, ",")
	for _, p := range parts {
		keyValue := strings.Split(strings.TrimSpace(p), "=")
		if len(keyValue) != 2 {
			level.Error(logger).Log(`Wrong constant labels format, should be "key=value"`, "input", p)
			continue
		}
		key := strings.TrimSpace(keyValue[0])
		value := strings.TrimSpace(keyValue[1])
		if key == "" || value == "" {
			continue
		}
		labels[key] = value
	}

	return labels
}

// NewExporter returns a new PostgreSQL exporter for the provided DSN.
func NewExporter(dsn []string, opts ...ExporterOpt) *Exporter {
	e := &Exporter{
		dsn:               dsn,
		builtinMetricMaps: builtinMetricMaps,
	}

	for _, opt := range opts {
		opt(e)
	}

	e.setupInternalMetrics()
	e.setupServers()

	return e
}

func (e *Exporter) setupServers() {
	e.servers = NewServers(ServerWithLabels(e.constantLabels))
}

func (e *Exporter) setupInternalMetrics() {
	e.duration = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   exporter,
		Name:        "last_scrape_duration_seconds",
		Help:        "Duration of the last scrape of metrics from PostgresSQL.",
		ConstLabels: e.constantLabels,
	})
	e.totalScrapes = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   exporter,
		Name:        "scrapes_total",
		Help:        "Total number of times PostgresSQL was scraped for metrics.",
		ConstLabels: e.constantLabels,
	})
	e.error = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   exporter,
		Name:        "last_scrape_error",
		Help:        "Whether the last scrape of metrics from PostgreSQL resulted in an error (1 for error, 0 for success).",
		ConstLabels: e.constantLabels,
	})
	e.psqlUp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "up",
		Help:        "Whether the last scrape of metrics from PostgreSQL was able to connect to the server (1 for yes, 0 for no).",
		ConstLabels: e.constantLabels,
	})
	e.userQueriesError = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   exporter,
		Name:        "user_queries_load_error",
		Help:        "Whether the user queries file was loaded and parsed successfully (1 for error, 0 for success).",
		ConstLabels: e.constantLabels,
	}, []string{"filename", "hashsum"})
}

// Describe implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
}

// Collect implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.scrape(ch)

	ch <- e.duration
	ch <- e.totalScrapes
	ch <- e.error
	ch <- e.psqlUp
	e.userQueriesError.Collect(ch)
}

func newDesc(subsystem, name, help string, labels prometheus.Labels) *prometheus.Desc {
	return prometheus.NewDesc(
		prometheus.BuildFQName(namespace, subsystem, name),
		help, nil, labels,
	)
}

func queryDatabases(server *Server) ([]string, error) {
	rows, err := server.db.Query("SELECT datname FROM pg_database WHERE datallowconn = true AND datistemplate = false AND datname != current_database()")
	if err != nil {
		return nil, fmt.Errorf("Error retrieving databases: %v", err)
	}
	defer rows.Close() // nolint: errcheck

	var databaseName string
	result := make([]string, 0)
	for rows.Next() {
		err = rows.Scan(&databaseName)
		if err != nil {
			return nil, errors.New(fmt.Sprintln("Error retrieving rows:", err))
		}
		result = append(result, databaseName)
	}

	return result, nil
}

// Query within a namespace mapping and emit metrics. Returns fatal errors if
// the scrape fails, and a slice of errors if they were non-fatal.
func queryNamespaceMapping(server *Server, namespace string, mapping MetricMapNamespace) ([]prometheus.Metric, []error, error) {
	// Check for a query override for this namespace
	query, found := server.queryOverrides[namespace]

	// Was this query disabled (i.e. nothing sensible can be queried on cu
	// version of PostgreSQL?
	if query == "" && found {
		// Return success (no pertinent data)
		return []prometheus.Metric{}, []error{}, nil
	}

	// Don't fail on a bad scrape of one metric
	var rows *sql.Rows
	var err error

	if !found {
		// I've no idea how to avoid this properly at the moment, but this is
		// an admin tool so you're not injecting SQL right?
		rows, err = server.db.Query(fmt.Sprintf("SELECT * FROM %s;", namespace)) // nolint: gas
	} else {
		rows, err = server.db.Query(query)
	}
	if err != nil {
		return []prometheus.Metric{}, []error{}, fmt.Errorf("Error running query on database %q: %s %v", server, namespace, err)
	}
	defer rows.Close() // nolint: errcheck

	var columnNames []string
	columnNames, err = rows.Columns()
	if err != nil {
		return []prometheus.Metric{}, []error{}, errors.New(fmt.Sprintln("Error retrieving column list for: ", namespace, err))
	}

	// Make a lookup map for the column indices
	var columnIdx = make(map[string]int, len(columnNames))
	for i, n := range columnNames {
		columnIdx[n] = i
	}

	var columnData = make([]interface{}, len(columnNames))
	var scanArgs = make([]interface{}, len(columnNames))
	for i := range columnData {
		scanArgs[i] = &columnData[i]
	}

	nonfatalErrors := []error{}

	metrics := make([]prometheus.Metric, 0)

	for rows.Next() {
		err = rows.Scan(scanArgs...)
		if err != nil {
			return []prometheus.Metric{}, []error{}, errors.New(fmt.Sprintln("Error retrieving rows:", namespace, err))
		}

		// Get the label values for this row.
		labels := make([]string, len(mapping.labels))
		for idx, label := range mapping.labels {
			labels[idx], _ = dbToString(columnData[columnIdx[label]])
		}

		// Loop over column names, and match to scan data. Unknown columns
		// will be filled with an untyped metric number *if* they can be
		// converted to float64s. NULLs are allowed and treated as NaN.
		for idx, columnName := range columnNames {
			var metric prometheus.Metric
			if metricMapping, ok := mapping.columnMappings[columnName]; ok {
				// Is this a metricy metric?
				if metricMapping.discard {
					continue
				}

				if metricMapping.histogram {
					var keys []float64
					err = pq.Array(&keys).Scan(columnData[idx])
					if err != nil {
						return []prometheus.Metric{}, []error{}, errors.New(fmt.Sprintln("Error retrieving", columnName, "buckets:", namespace, err))
					}

					var values []int64
					valuesIdx, ok := columnIdx[columnName+"_bucket"]
					if !ok {
						nonfatalErrors = append(nonfatalErrors, errors.New(fmt.Sprintln("Missing column: ", namespace, columnName+"_bucket")))
						continue
					}
					err = pq.Array(&values).Scan(columnData[valuesIdx])
					if err != nil {
						return []prometheus.Metric{}, []error{}, errors.New(fmt.Sprintln("Error retrieving", columnName, "bucket values:", namespace, err))
					}

					buckets := make(map[float64]uint64, len(keys))
					for i, key := range keys {
						if i >= len(values) {
							break
						}
						buckets[key] = uint64(values[i])
					}

					idx, ok = columnIdx[columnName+"_sum"]
					if !ok {
						nonfatalErrors = append(nonfatalErrors, errors.New(fmt.Sprintln("Missing column: ", namespace, columnName+"_sum")))
						continue
					}
					sum, ok := dbToFloat64(columnData[idx])
					if !ok {
						nonfatalErrors = append(nonfatalErrors, errors.New(fmt.Sprintln("Unexpected error parsing column: ", namespace, columnName+"_sum", columnData[idx])))
						continue
					}

					idx, ok = columnIdx[columnName+"_count"]
					if !ok {
						nonfatalErrors = append(nonfatalErrors, errors.New(fmt.Sprintln("Missing column: ", namespace, columnName+"_count")))
						continue
					}
					count, ok := dbToUint64(columnData[idx])
					if !ok {
						nonfatalErrors = append(nonfatalErrors, errors.New(fmt.Sprintln("Unexpected error parsing column: ", namespace, columnName+"_count", columnData[idx])))
						continue
					}

					metric = prometheus.MustNewConstHistogram(
						metricMapping.desc,
						count, sum, buckets,
						labels...,
					)
				} else {
					value, ok := dbToFloat64(columnData[idx])
					if !ok {
						nonfatalErrors = append(nonfatalErrors, errors.New(fmt.Sprintln("Unexpected error parsing column: ", namespace, columnName, columnData[idx])))
						continue
					}
					// Generate the metric
					metric = prometheus.MustNewConstMetric(metricMapping.desc, metricMapping.vtype, value, labels...)
				}
			} else {
				// Unknown metric. Report as untyped if scan to float64 works, else note an error too.
				metricLabel := fmt.Sprintf("%s_%s", namespace, columnName)
				desc := prometheus.NewDesc(metricLabel, fmt.Sprintf("Unknown metric from %s", namespace), mapping.labels, server.labels)

				// Its not an error to fail here, since the values are
				// unexpected anyway.
				value, ok := dbToFloat64(columnData[idx])
				if !ok {
					nonfatalErrors = append(nonfatalErrors, errors.New(fmt.Sprintln("Unparseable column type - discarding: ", namespace, columnName, err)))
					continue
				}
				metric = prometheus.MustNewConstMetric(desc, prometheus.UntypedValue, value, labels...)
			}
			metrics = append(metrics, metric)
		}
	}
	return metrics, nonfatalErrors, nil
}

// Iterate through all the namespace mappings in the exporter and run their
// queries.
func queryNamespaceMappings(ch chan<- prometheus.Metric, server *Server) map[string]error {
	// Return a map of namespace -> errors
	namespaceErrors := make(map[string]error)

	scrapeStart := time.Now()

	for namespace, mapping := range server.metricMap {
		level.Debug(logger).Log("msg", "Querying namespace", "namespace", namespace)

		if mapping.master && !server.master {
			level.Debug(logger).Log("msg", "Query skipped...")
			continue
		}

		// check if the query is to be run on specific database server version range or not
		if len(server.runonserver) > 0 {
			serVersion, _ := semver.Parse(server.lastMapVersion.String())
			runServerRange, _ := semver.ParseRange(server.runonserver)
			if !runServerRange(serVersion) {
				level.Debug(logger).Log("msg", "Query skipped for this database version", "version", server.lastMapVersion.String(), "target_version", server.runonserver)
				continue
			}
		}

		scrapeMetric := false
		// Check if the metric is cached
		server.cacheMtx.Lock()
		cachedMetric, found := server.metricCache[namespace]
		server.cacheMtx.Unlock()
		// If found, check if needs refresh from cache
		if found {
			if scrapeStart.Sub(cachedMetric.lastScrape).Seconds() > float64(mapping.cacheSeconds) {
				scrapeMetric = true
			}
		} else {
			scrapeMetric = true
		}

		var metrics []prometheus.Metric
		var nonFatalErrors []error
		var err error
		if scrapeMetric {
			metrics, nonFatalErrors, err = queryNamespaceMapping(server, namespace, mapping)
		} else {
			metrics = cachedMetric.metrics
		}

		// Serious error - a namespace disappeared
		if err != nil {
			namespaceErrors[namespace] = err
			level.Info(logger).Log("err", err)
		}
		// Non-serious errors - likely version or parsing problems.
		if len(nonFatalErrors) > 0 {
			for _, err := range nonFatalErrors {
				level.Info(logger).Log("err", err)
			}
		}

		// Emit the metrics into the channel
		for _, metric := range metrics {
			ch <- metric
		}

		if scrapeMetric {
			// Only cache if metric is meaningfully cacheable
			if mapping.cacheSeconds > 0 {
				server.cacheMtx.Lock()
				server.metricCache[namespace] = cachedMetrics{
					metrics:    metrics,
					lastScrape: scrapeStart,
				}
				server.cacheMtx.Unlock()
			}
		}
	}

	return namespaceErrors
}

// Check and update the exporters query maps if the version has changed.
func (e *Exporter) checkMapVersions(ch chan<- prometheus.Metric, server *Server) error {
	level.Debug(logger).Log("msg", "Querying PostgreSQL version", "server", server)
	versionRow := server.db.QueryRow("SELECT version();")
	var versionString string
	err := versionRow.Scan(&versionString)
	if err != nil {
		return fmt.Errorf("Error scanning version string on %q: %v", server, err)
	}
	semanticVersion, err := parseVersion(versionString)
	if err != nil {
		return fmt.Errorf("Error parsing version string on %q: %v", server, err)
	}
	if !e.disableDefaultMetrics && semanticVersion.LT(lowestSupportedVersion) {
		level.Warn(logger).Log("msg", "PostgreSQL version is lower than our lowest supported version", "server", server, "version", semanticVersion, "lowest_supported_version", lowestSupportedVersion)
	}

	// Check if semantic version changed and recalculate maps if needed.
	if semanticVersion.NE(server.lastMapVersion) || server.metricMap == nil {
		level.Info(logger).Log("msg", "Semantic version changed", "server", server, "from", server.lastMapVersion, "to", semanticVersion)
		server.mappingMtx.Lock()

		// Get Default Metrics only for master database
		if !e.disableDefaultMetrics && server.master {
			server.metricMap = makeDescMap(semanticVersion, server.labels, e.builtinMetricMaps)
			server.queryOverrides = makeQueryOverrideMap(semanticVersion, queryOverrides)
		} else {
			server.metricMap = make(map[string]MetricMapNamespace)
			server.queryOverrides = make(map[string]string)
		}

		server.lastMapVersion = semanticVersion

		if e.userQueriesPath != "" {
			// Clear the metric while a reload is happening
			e.userQueriesError.Reset()

			// Calculate the hashsum of the useQueries
			userQueriesData, err := ioutil.ReadFile(e.userQueriesPath)
			if err != nil {
				level.Error(logger).Log("msg", "Failed to reload user queries", "path", e.userQueriesPath, "err", err)
				e.userQueriesError.WithLabelValues(e.userQueriesPath, "").Set(1)
			} else {
				hashsumStr := fmt.Sprintf("%x", sha256.Sum256(userQueriesData))

				if err := addQueries(userQueriesData, semanticVersion, server); err != nil {
					level.Error(logger).Log("msg", "Failed to reload user queries", "path", e.userQueriesPath, "err", err)
					e.userQueriesError.WithLabelValues(e.userQueriesPath, hashsumStr).Set(1)
				} else {
					// Mark user queries as successfully loaded
					e.userQueriesError.WithLabelValues(e.userQueriesPath, hashsumStr).Set(0)
				}
			}
		}

		server.mappingMtx.Unlock()
	}

	// Output the version as a special metric only for master database
	versionDesc := prometheus.NewDesc(fmt.Sprintf("%s_%s", namespace, staticLabelName),
		"Version string as reported by postgres", []string{"version", "short_version"}, server.labels)

	if !e.disableDefaultMetrics && server.master {
		ch <- prometheus.MustNewConstMetric(versionDesc,
			prometheus.UntypedValue, 1, versionString, semanticVersion.String())
	}
	return nil
}

func (e *Exporter) scrape(ch chan<- prometheus.Metric) {
	defer func(begun time.Time) {
		e.duration.Set(time.Since(begun).Seconds())
	}(time.Now())

	e.totalScrapes.Inc()

	dsns := e.dsn
	if e.autoDiscoverDatabases {
		dsns = e.discoverDatabaseDSNs()
	}

	var errorsCount int
	var connectionErrorsCount int

	for _, dsn := range dsns {
		if err := e.scrapeDSN(ch, dsn); err != nil {
			errorsCount++

			level.Error(logger).Log("err", err)

			if _, ok := err.(*ErrorConnectToServer); ok {
				connectionErrorsCount++
			}
		}
	}

	switch {
	case connectionErrorsCount >= len(dsns):
		e.psqlUp.Set(0)
	default:
		e.psqlUp.Set(1) // Didn't fail, can mark connection as up for this scrape.
	}

	switch errorsCount {
	case 0:
		e.error.Set(0)
	default:
		e.error.Set(1)
	}
}

func (e *Exporter) discoverDatabaseDSNs() []string {
	// connstring syntax is complex (and not sure if even regular).
	// we don't need to parse it, so just superficially validate that it starts
	// with a valid-ish keyword pair
	connstringRe := regexp.MustCompile(`^ *[a-zA-Z0-9]+ *= *[^= ]+`)

	dsns := make(map[string]struct{})
	for _, dsn := range e.dsn {
		var dsnURI *url.URL
		var dsnConnstring string

		if strings.HasPrefix(dsn, "postgresql://") {
			var err error
			dsnURI, err = url.Parse(dsn)
			if err != nil {
				level.Error(logger).Log("msg", "Unable to parse DSN as URI", "dsn", loggableDSN(dsn), "err", err)
				continue
			}
		} else if connstringRe.MatchString(dsn) {
			dsnConnstring = dsn
		} else {
			level.Error(logger).Log("msg", "Unable to parse DSN as either URI or connstring", "dsn", loggableDSN(dsn))
			continue
		}

		server, err := e.servers.GetServer(dsn)
		if err != nil {
			level.Error(logger).Log("msg", "Error opening connection to database", "dsn", loggableDSN(dsn), "err", err)
			continue
		}
		dsns[dsn] = struct{}{}

		// If autoDiscoverDatabases is true, set first dsn as master database (Default: false)
		server.master = true

		databaseNames, err := queryDatabases(server)
		if err != nil {
			level.Error(logger).Log("msg", "Error querying databases", "dsn", loggableDSN(dsn), "err", err)
			continue
		}
		for _, databaseName := range databaseNames {
			if contains(e.excludeDatabases, databaseName) {
				continue
			}

			if len(e.includeDatabases) != 0 && !contains(e.includeDatabases, databaseName) {
				continue
			}

			if dsnURI != nil {
				dsnURI.Path = databaseName
				dsn = dsnURI.String()
			} else {
				// replacing one dbname with another is complicated.
				// just append new dbname to override.
				dsn = fmt.Sprintf("%s dbname=%s", dsnConnstring, databaseName)
			}
			dsns[dsn] = struct{}{}
		}
	}

	result := make([]string, len(dsns))
	index := 0
	for dsn := range dsns {
		result[index] = dsn
		index++
	}

	return result
}

func (e *Exporter) scrapeDSN(ch chan<- prometheus.Metric, dsn string) error {
	server, err := e.servers.GetServer(dsn)

	if err != nil {
		return &ErrorConnectToServer{fmt.Sprintf("Error opening connection to database (%s): %s", loggableDSN(dsn), err.Error())}
	}

	// Check if autoDiscoverDatabases is false, set dsn as master database (Default: false)
	if !e.autoDiscoverDatabases {
		server.master = true
	}

	// Check if map versions need to be updated
	if err := e.checkMapVersions(ch, server); err != nil {
		level.Warn(logger).Log("msg", "Proceeding with outdated query maps, as the Postgres version could not be determined", "err", err)
	}

	return server.Scrape(ch, e.disableSettingsMetrics)
}

// try to get the DataSource
// DATA_SOURCE_NAME always wins so we do not break older versions
// reading secrets from files wins over secrets in environment variables
// DATA_SOURCE_NAME > DATA_SOURCE_{USER|PASS}_FILE > DATA_SOURCE_{USER|PASS}
func getDataSources() ([]string, error) {
	var dsn = os.Getenv("DATA_SOURCE_NAME")
	if len(dsn) != 0 {
		return strings.Split(dsn, ","), nil
	}

	var user, pass, uri string

	dataSourceUserFile := os.Getenv("DATA_SOURCE_USER_FILE")
	if len(dataSourceUserFile) != 0 {
		fileContents, err := ioutil.ReadFile(dataSourceUserFile)
		if err != nil {
			return nil, fmt.Errorf("failed loading data source user file %s: %s", dataSourceUserFile, err.Error())
		}
		user = strings.TrimSpace(string(fileContents))
	} else {
		user = os.Getenv("DATA_SOURCE_USER")
	}

	dataSourcePassFile := os.Getenv("DATA_SOURCE_PASS_FILE")
	if len(dataSourcePassFile) != 0 {
		fileContents, err := ioutil.ReadFile(dataSourcePassFile)
		if err != nil {
			return nil, fmt.Errorf("failed loading data source pass file %s: %s", dataSourcePassFile, err.Error())
		}
		pass = strings.TrimSpace(string(fileContents))
	} else {
		pass = os.Getenv("DATA_SOURCE_PASS")
	}

	ui := url.UserPassword(user, pass).String()
	dataSrouceURIFile := os.Getenv("DATA_SOURCE_URI_FILE")
	if len(dataSrouceURIFile) != 0 {
		fileContents, err := ioutil.ReadFile(dataSrouceURIFile)
		if err != nil {
			return nil, fmt.Errorf("failed loading data source URI file %s: %s", dataSrouceURIFile, err.Error())
		}
		uri = strings.TrimSpace(string(fileContents))
	} else {
		uri = os.Getenv("DATA_SOURCE_URI")
	}

	dsn = "postgresql://" + ui + "@" + uri

	return []string{dsn}, nil
}

func contains(a []string, x string) bool {
	for _, n := range a {
		if x == n {
			return true
		}
	}
	return false
}

func main() {
	kingpin.Version(version.Print(exporterName))
	promlogConfig := &promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, promlogConfig)
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	logger = promlog.New(promlogConfig)

	// landingPage contains the HTML served at '/'.
	// TODO: Make this nicer and more informative.
	var landingPage = []byte(`<html>
	<head><title>Postgres exporter</title></head>
	<body>
	<h1>Postgres exporter</h1>
	<p><a href='` + *metricPath + `'>Metrics</a></p>
	</body>
	</html>
	`)

	if *onlyDumpMaps {
		dumpMaps()
		return
	}

	dsn, err := getDataSources()
	if err != nil {
		level.Error(logger).Log("msg", "Failed reading data sources", "err", err.Error())
		os.Exit(1)
	}

	if len(dsn) == 0 {
		level.Error(logger).Log("msg", "Couldn't find environment variables describing the datasource to use")
		os.Exit(1)
	}

	opts := []ExporterOpt{
		DisableDefaultMetrics(*disableDefaultMetrics),
		DisableSettingsMetrics(*disableSettingsMetrics),
		AutoDiscoverDatabases(*autoDiscoverDatabases),
		WithUserQueriesPath(*queriesPath),
		WithConstantLabels(*constantLabelsList),
		ExcludeDatabases(*excludeDatabases),
		IncludeDatabases(*includeDatabases),
	}

	exporter := NewExporter(dsn, opts...)
	defer func() {
		exporter.servers.Close()
	}()

	prometheus.MustRegister(version.NewCollector(exporterName))

	prometheus.MustRegister(exporter)

	http.Handle(*metricPath, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8") // nolint: errcheck
		w.Write(landingPage)                                       // nolint: errcheck
	})

	level.Info(logger).Log("msg", "Listening on address", "address", *listenAddress)
	srv := &http.Server{Addr: *listenAddress}
	if err := web.ListenAndServe(srv, *webConfig, logger); err != nil {
		level.Error(logger).Log("msg", "Error running HTTP server", "err", err)
		os.Exit(1)
	}
}
