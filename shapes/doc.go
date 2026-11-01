// Package shapes holds the Go types for protos/shapes.proto: the messages
// persona-beacon exchanges over UDP multicast on the microVM bridge and with
// the host's broker. The beacon (C#) is generated from the same file.
package shapes

//go:generate sh -c "cd .. && protoc --plugin=protoc-gen-go=$(go tool -n protoc-gen-go) --go_out=. --go_opt=module=github.com/johndavisdev/persona protos/shapes.proto"
