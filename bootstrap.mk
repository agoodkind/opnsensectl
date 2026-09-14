# DO NOT MODIFY.
#
# Owned by go-makefile. This shim fetches the engine and includes go.mk.
# Refresh with: go-mk scaffold
# https://github.com/agoodkind/go-makefile


GO_MK_DEV_DIR  ?=
GO_MK_MODULES  ?=
GO_MK          := .make/go.mk
GO_MK_BASE_URL ?= https://raw.githubusercontent.com/agoodkind/go-makefile/main
GO_MK_API_REPO ?= agoodkind/go-makefile
GO_MK_API_REF  ?= main

GO_MK_BOOTSTRAP := .make/scripts/go-mk-bootstrap.sh
GO_MK_BOOTSTRAP_BASE_URL ?= https://raw.githubusercontent.com
GO_MK_BOOTSTRAP_URL := $(GO_MK_BOOTSTRAP_BASE_URL)/$(GO_MK_API_REPO)/$(GO_MK_API_REF)/scripts/go-mk-bootstrap.sh

define _go_mk_get_bootstrap
	if [ -n "$(GO_MK_DEV_DIR)" ] && [ -f "$(GO_MK_DEV_DIR)/scripts/go-mk-bootstrap.sh" ]; then \
		mkdir -p .make/scripts; \
		devtmp=$$(mktemp "$(GO_MK_BOOTSTRAP).tmp.XXXXXX") || exit 1; \
		if cp "$(GO_MK_DEV_DIR)/scripts/go-mk-bootstrap.sh" "$$devtmp" && mv "$$devtmp" "$(GO_MK_BOOTSTRAP)"; then \
			: ; \
		else \
			rm -f "$$devtmp"; \
			printf '%s\n' "error: could not install $(GO_MK_BOOTSTRAP) from GO_MK_DEV_DIR=$(GO_MK_DEV_DIR)" >&2; \
			exit 1; \
		fi; \
	elif [ -s "$(GO_MK_BOOTSTRAP)" ]; then \
		: ; \
	else \
		mkdir -p .make/scripts; \
		tmp=$$(mktemp "$(GO_MK_BOOTSTRAP).tmp.XXXXXX") || exit 1; \
		if curl -fsSL --connect-timeout 5 --max-time 15 \
			--speed-limit 1024 --speed-time 3 \
			--retry 3 --retry-delay 2 --retry-max-time 4 \
			"$(GO_MK_BOOTSTRAP_URL)" -o "$$tmp" 2>/dev/null && [ -s "$$tmp" ]; then \
			mv "$$tmp" "$(GO_MK_BOOTSTRAP)"; \
		else \
			rm -f "$$tmp"; \
			printf '%s\n' "error: could not obtain $(GO_MK_BOOTSTRAP). Set GO_MK_DEV_DIR, or check network access to $(GO_MK_BOOTSTRAP_BASE_URL)" >&2; \
			exit 1; \
		fi; \
	fi; \
	chmod +x "$(GO_MK_BOOTSTRAP)"
endef

GO_MK_BOOTSTRAP_FETCHED := 1

ifeq ($(strip $(_GO_MK_PROVISIONED)),1)
GO_MK_BOOTSTRAP_PRESENT := $(shell test -s "$(GO_MK_BOOTSTRAP)" && printf yes)
$(if $(GO_MK_BOOTSTRAP_PRESENT),,$(error go-makefile expected a non-empty $(GO_MK_BOOTSTRAP); rerun without _GO_MK_PROVISIONED))
else
$(shell { $(call _go_mk_get_bootstrap); } 1>&2)
endif

GO_MK_PROVISION := $(shell GO_MK_API_REPO="$(GO_MK_API_REPO)" GO_MK_API_REF="$(GO_MK_API_REF)" GO_MK_MODULES="$(GO_MK_MODULES)" GO_MK_CODELOAD_BASE="$(GO_MK_CODELOAD_BASE)" GO_MK_DEV_DIR="$(GO_MK_DEV_DIR)" _GO_MK_PROVISIONED="$(_GO_MK_PROVISIONED)" bash "$(GO_MK_BOOTSTRAP)" >&2 && printf ok)
$(if $(filter ok,$(GO_MK_PROVISION)),,$(error go-makefile failed to provision its assets))

-include $(GO_MK)
