# proto

golem's APIs (architecture.md §3), generated into `../gen` for Go:

    go install github.com/bufbuild/buf/cmd/buf@latest google.golang.org/protobuf/cmd/protoc-gen-go@latest connectrpc.com/connect/cmd/protoc-gen-connect-go@latest
    cd proto && buf lint && buf generate

The generated code is committed; regenerate it with the proto change.
