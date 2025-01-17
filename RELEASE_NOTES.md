# Release Notes

## Version 9.1.3 (16 March 2021)

### New Features and Behavioral Changes

* [https://issues.couchbase.com/browse/GOCBC-1056](GOCBC-1056):
  Various performance improvements to reduce CPU level.
* [https://issues.couchbase.com/browse/GOCBC-1068)](GOCBC-1068):
  Dropped the log level for preempted config updates.
* [https://issues.couchbase.com/browse/GOCBC-940)](GOCBC-940):
  Updated the tracing interfaces and orphaned response logging output.

### Fixed Issues

* [https://issues.couchbase.com/browse/GOCBC-1066](GOCBC-1066):
  Fixed issue which could cause the config pollers to panic.

## Version 9.1.2 (16 February 2021)

### New Features and Behavioral Changes

* [https://issues.couchbase.com/browse/GOCBC-1041](GOCBC-1041):
  Dropped the log level for memdclient read failures to warn, from error.
* [https://issues.couchbase.com/browse/GOCBC-1046](GOCBC-1046):
  Added `MaxTTl` to `ManifestCollection`.

### Fixed Issues

* [https://issues.couchbase.com/browse/GOCBC-1042](GOCBC-1042):
  Fixed issue where bucket names were not being correctly escaped.
* [https://issues.couchbase.com/browse/GOCBC-1050](GOCBC-1050):
  Fixed issue where the diagnostics component could panic if an operation was cancelled by the user after it had already been internally cancelled.

## Version 9.1.1 (19 January 2021)

### New Features and Behavioral Changes

* [https://issues.couchbase.com/browse/GOCBC-1032](GOCBC-1032):
  Added support for bucket capability support verification to agent, at API stability internal.
* [https://issues.couchbase.com/browse/GOCBC-1030](GOCBC-1030):
  Added support for internal cancellation of bootstrap before completion, allowing pipeline clients to shutdown without waiting for bootstrap to complete (such as on connection takeover).

  Added support to fallback to http config fetching if select bucket fails with a valid fallback error, allowing for faster config fetching against non-kv nodes.

## Version 9.1.0 (15 December 2020)

### New Features and Behavioral Changes

* [https://issues.couchbase.com/browse/GOCBC-854](GOCBC-854):
Added support for user impersonation.
* [https://issues.couchbase.com/browse/GOCBC-1013](GOCBC-1013):
Added support for `StatsKeys` and `StatsChunks` to `SingleServerStats` to support responses for stats keys such as `connections` which contain complex objects per packet.

### Fixed Issues

* [https://issues.couchbase.com/browse/GOCBC-1016](GOCBC-1016):
Fixed issue where creating an agent with no bucket and a non-default port HTTP address could lead to a panic in `WaitForReady`.
(Note: `WaitForReady` will *never* return success in this scenario)
* [https://issues.couchbase.com/browse/GOCBC-1028](GOCBC-1028):
Fixed issue where bootstrapping against a non-kv node could never successfully fully connect.
