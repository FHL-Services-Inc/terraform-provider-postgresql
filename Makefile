TEST?=$$(go list ./...)
GOFMT_FILES?=$$(find . -name '*.go')
PKG_NAME=postgresql
# Keep in sync with the golangci-lint version in .github/workflows/golangci.yml
GOLANGCI_LINT_VERSION?=v2.12.2

default: build

build: fmtcheck
	go install

test: fmtcheck
	go test $(TEST) || exit 1
	echo $(TEST) | \
		xargs -t -n4 go test $(TESTARGS) -timeout=30s -parallel=4

testacc_setup: fmtcheck
	@sh -c "'$(CURDIR)/tests/testacc_setup.sh'"

testacc_cleanup: fmtcheck
	@sh -c "'$(CURDIR)/tests/testacc_cleanup.sh'"

testacc: fmtcheck
	@sh -c "'$(CURDIR)/tests/testacc_full.sh'"

# Same as testacc_setup/testacc/testacc_cleanup, but with a stock postgres
# image and Apple's `container` CLI by default (CONTAINER_CLI=docker also
# works). See tests/container_acc.sh for the settings.
testacc_container_setup:
	@'$(CURDIR)/tests/container_acc.sh' start $(PROFILE)

testacc_container_cleanup:
	@'$(CURDIR)/tests/container_acc.sh' stop

testacc_container: fmtcheck
	@'$(CURDIR)/tests/container_acc.sh' run $(TESTARGS)

.PHONY: testacc_container testacc_container_setup testacc_container_cleanup

vet:
	@echo "go vet ."
	@go vet $$(go list ./...) ; if [ $$? -eq 1 ]; then \
		echo ""; \
		echo "Vet found suspicious constructs. Please check the reported constructs"; \
		echo "and fix them if necessary before submitting the code for review."; \
		exit 1; \
	fi

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

fmt:
	gofmt -w $(GOFMT_FILES)

fmtcheck:
	@sh -c "'$(CURDIR)/scripts/gofmtcheck.sh'"

.PHONY: build test testacc vet lint fmt fmtcheck

