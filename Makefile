LCP_REPO ?= ./lcp
LCP_PROTO ?= $(LCP_REPO)/proto/definitions
protoVer=0.13.1
protoImageName=ghcr.io/cosmos/proto-builder:$(protoVer)
protoImage=docker run --user 0 --rm -v $(CURDIR):/workspace --workdir /workspace $(protoImageName)

.PHONY: yrly
yrly:
	go build -o ./bin/yrly -tags customcert ./relay/bin

.PHONY: lcp
lcp:
	$(MAKE) -C $(LCP_REPO) -B && mv $(LCP_REPO)/bin/* ./bin/

.PHONY: e2e-test
e2e-test: yrly lcp
	./scripts/run_e2e_test.sh

.PHONY: proto-gen proto-update-deps
proto-gen:
	@echo "Generating Protobuf files"
	@rm -rf ./proto/ibc && rm -rf ./proto/lcp
	@mkdir -p ./proto/ibc/lightclients/lcp/v1 && mkdir -p ./proto/lcp/service/elc/v1
	@sed "s/option\sgo_package.*;/option\ go_package\ =\ \"github.com\/datachainlab\/lcp-go\/light-clients\/lcp\/types\";/g"\
		$(LCP_PROTO)/ibc/lightclients/lcp/v1/lcp.proto > ./proto/ibc/lightclients/lcp/v1/lcp.proto
	@sed "s/option\sgo_package.*;/option\ go_package\ =\ \"github.com\/datachainlab\/lcp-go\/relay\/elc\";/g"\
		$(LCP_PROTO)/lcp/service/elc/v1/query.proto > ./proto/lcp/service/elc/v1/query.proto
	@sed "s/option\sgo_package.*;/option\ go_package\ =\ \"github.com\/datachainlab\/lcp-go\/relay\/elc\";/g"\
		$(LCP_PROTO)/lcp/service/elc/v1/tx.proto > ./proto/lcp/service/elc/v1/tx.proto
	@$(protoImage) sh ./scripts/protocgen.sh

proto-update-deps:
	@echo "Updating Protobuf dependencies"
	$(DOCKER) run --user 0 --rm -v $(CURDIR)/proto:/workspace --workdir /workspace $(protoImageName) buf mod update
