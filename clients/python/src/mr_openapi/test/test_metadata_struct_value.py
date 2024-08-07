"""Model Registry REST API.

REST API for Model Registry to create and manage ML model metadata

The version of the OpenAPI document: v1alpha3
Generated by OpenAPI Generator (https://openapi-generator.tech)

Do not edit the class manually.
"""  # noqa: E501

import unittest

from mr_openapi.models.metadata_struct_value import MetadataStructValue


class TestMetadataStructValue(unittest.TestCase):
    """MetadataStructValue unit test stubs."""

    def setUp(self):
        pass

    def tearDown(self):
        pass

    def make_instance(self, include_optional) -> MetadataStructValue:
        """Test MetadataStructValue
        include_option is a boolean, when False only required
        params are included, when True both required and
        optional params are included.
        """
        # uncomment below to create an instance of `MetadataStructValue`
        """
        model = MetadataStructValue()
        if include_optional:
            return MetadataStructValue(
                struct_value = '',
                metadata_type = 'MetadataStructValue'
            )
        else:
            return MetadataStructValue(
                struct_value = '',
                metadata_type = 'MetadataStructValue',
        )
        """

    def testMetadataStructValue(self):
        """Test MetadataStructValue."""
        # inst_req_only = self.make_instance(include_optional=False)
        # inst_req_and_optional = self.make_instance(include_optional=True)


if __name__ == "__main__":
    unittest.main()
