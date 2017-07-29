# Goal

Faster implementation of csv encoding/decoding for the specific case of ASCII
documents, no UTF requirement.

One of the issues available in
the
[Go London User Group Hacknight (July)](https://www.meetup.com/Go-London-User-Group/events/241545817/)

# Reason

Working with large amounts of data coming from legacy applications where
encoding is known to be ASCII. A big amount of time in `encoding/csv` is spent
in rune-level operations which are not needed if data is known to be ASCII.

# Proposed fix

Use the base code in `encoding/csv`, change runes for bytes

# Result 1

~25% speed up

# Subsequent fix

Inline `buffer/bytes` from go 1.9 source and use a larger base buffer

# Result 2

~15% additional speed up. Not sure if because of inlining, 1.9 having a better
`bytes/buffer` implementation, larger base buffer or all three reasons.

# Comparison

```
go test -bench ReadReuseRecordLargeFields -benchtime=30s

BenchmarkReadReuseRecordLargeFields-4   	 1000000	     68351 ns/op	    2976 B/op	      12 allocs/op
PASS
ok  	normal_csv	69.011s
```

```
go test -bench ReadReuseRecordLargeFields -benchtime=30s
BenchmarkReadReuseRecordLargeFields-4   	 1000000	     38828 ns/op	    2976 B/op	      12 allocs/op
PASS
ok  	quickcsv	39.243s
```
