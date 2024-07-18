"""Model Registry REST API.

REST API for Model Registry to create and manage ML model metadata

The version of the OpenAPI document: v1alpha3
Generated by OpenAPI Generator (https://openapi-generator.tech)

Do not edit the class manually.
"""  # noqa: E501

import unittest

from mr_openapi.models.inference_service_create import InferenceServiceCreate


class TestInferenceServiceCreate(unittest.TestCase):
    """InferenceServiceCreate unit test stubs."""

    def setUp(self):
        pass

    def tearDown(self):
        pass

    def make_instance(self, include_optional) -> InferenceServiceCreate:
        """Test InferenceServiceCreate
        include_option is a boolean, when False only required
        params are included, when True both required and
        optional params are included.
        """
        # uncomment below to create an instance of `InferenceServiceCreate`
        """
        model = InferenceServiceCreate()
        if include_optional:
            return InferenceServiceCreate(
                custom_properties = {
                    'key' : null
                    },
                description = '',
                external_id = '',
                name = '',
                model_version_id = '',
                runtime = '',
                desired_state = 'DEPLOYED',
                registered_model_id = '',
                serving_environment_id = ''
            )
        else:
            return InferenceServiceCreate(
                registered_model_id = '',
                serving_environment_id = '',
        )
        """

    def testInferenceServiceCreate(self):
        """Test InferenceServiceCreate."""
        # inst_req_only = self.make_instance(include_optional=False)
        # inst_req_and_optional = self.make_instance(include_optional=True)


if __name__ == "__main__":
    unittest.main()