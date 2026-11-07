.PHONY: verify-vendor curator-command

# Both targets require an explicit build checkout; main carries no vendor tree.
verify-vendor:
	go run -mod=readonly ./tools/vendorcheck --repository "$(BUILD_REPOSITORY)"

curator-command:
	@test -n "$(BUILD_REPOSITORY)" -a -n "$(OUTPUT)" || { echo 'BUILD_REPOSITORY and OUTPUT are required' >&2; exit 1; }
	CGO_ENABLED=0 GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOENV=off go -C "$(BUILD_REPOSITORY)" build -mod=vendor -trimpath -buildvcs=false -buildmode=exe -compiler=gc -pgo=off -ldflags='-linkmode=internal -libgcc=none' -o "$(abspath $(OUTPUT))" ./cmd/a2a
