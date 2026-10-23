import json
import string

import pytest
import schemathesis
from hypothesis import settings
from hypothesis import strategies as st

from tests.fuzz_api.model_registry.test_mr_stateless import call_and_validate_with_null_byte_handling

schema = schemathesis.pytest.from_fixture("generated_schema")
PREVIEW_PATH = "/api/model_catalog/v1/sources/preview"

@pytest.mark.parametrize("generated_schema", ["catalog-v1.yaml"], indirect=True)
@schema.exclude(path=PREVIEW_PATH, method="POST").parametrize()
@settings(
    deadline=None,
)
@pytest.mark.fuzz
def test_catalog_api_stateless(auth_headers: dict, case: schemathesis.Case, verify_ssl: bool) -> None:
    """Test the Model Catalog API endpoints.

    This test uses schemathesis to generate and validate API requests
    """
    call_and_validate_with_null_byte_handling(case, auth_headers, verify_ssl)


@pytest.mark.parametrize("generated_schema", ["catalog-v1.yaml"], indirect=True)
@schema.include(path=PREVIEW_PATH, method="POST").parametrize()
@schema.given(
    model_names=st.lists(
        st.text(alphabet=string.ascii_letters + string.digits + "-_/", min_size=1, max_size=50),
        max_size=10,
        unique=True,
    ),
    included_models=st.lists(
        st.text(alphabet=string.ascii_letters + string.digits + "-_/*", min_size=1, max_size=20),
        max_size=3,
    ),
)
@settings(deadline=None)
@pytest.mark.fuzz
def test_catalog_source_preview(
    auth_headers: dict, case: schemathesis.Case, verify_ssl: bool,
    model_names: list[str], included_models: list[str],
) -> None:
    """Fuzz preview with valid YAML files and generated query parameters.

    The binary format in OpenAPI cannot describe the required YAML contents.
    JSON is a YAML subset, so encode generated source and catalog documents as
    JSON rather than sending the arbitrary binary files Schemathesis generates.
    """
    case.media_type = "multipart/form-data"
    case.body = {
        "config": json.dumps({"type": "yaml", "includedModels": included_models}),
        "catalogData": json.dumps({"models": [{"name": name} for name in model_names]}),
    }
    case.call_and_validate(headers=auth_headers, verify=verify_ssl)
