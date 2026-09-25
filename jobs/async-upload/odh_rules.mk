.PHONY: deploy-mr-odh
deploy-mr-odh:
	cd ../../ && ./scripts/deploy_on_odh.sh

.PHONY: undeploy-mr-odh
undeploy-mr-odh:
	cd ../../ && ./scripts/undeploy_on_odh.sh

.PHONY: undeploy-seaweedfs
undeploy-seaweedfs:
	@echo "Undeploy SeaweedFS..."
	cd ../../ && ./scripts/undeploy_seaweedfs.sh

.PHONY: test-e2e-odh-async-jobs
test-e2e-odh-async-jobs: deploy-mr-odh deploy-local-registry deploy-test-seaweedfs
	@echo "Running Async Jobs e2e tests..."
	@( \
	trap 'rm -f ../../scripts/manifests/seaweedfs/.env' EXIT; \
	set -a; . ../../scripts/manifests/seaweedfs/.env; set +a; \
	mkdir -p ../../results; \
	. ../../scripts/odh_env.sh && \
	poetry install --all-extras --with integration && \
	poetry run pytest --e2e tests/integration/ -svvv -rA \
		--html=../../results/report.html \
		--junit-xml=../../results/xunit_report.xml \
		--self-contained-html \
		-o junit_suite_name=odh-async-job \
	)

.PHONY: test-e2e-odh-async-jobs-cleanup
test-e2e-odh-async-jobs-cleanup: undeploy-mr-odh undeploy-seaweedfs undeploy-local-kind-registry
	@echo "Cleaning up port-forward processes..."
	@if [ -f .port-forwards.pid ]; then \
		kill $$(cat .port-forwards.pid) || true; \
		rm -f .port-forwards.pid; \
	fi
