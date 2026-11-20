import ssl
import sys
from pathlib import Path
import unittest
from unittest.mock import patch
import urllib.request

sys.path.insert(0, str(Path(__file__).parents[1] / 'lib'))
from ci_https import context_for, StorageHTTPSHandler


class StorageTLSTests(unittest.TestCase):
    def test_handler_arguments_are_accepted_by_the_actual_python_runtime(self):
        handler = StorageHTTPSHandler()
        def create(connection_class, request, **kwargs):
            connection = connection_class('productionresultssa16.blob.core.windows.net', **kwargs)
            self.assertTrue(connection._context.check_hostname)
            connection.close()
            return 'constructed'
        with patch.object(handler, 'do_open', side_effect=create):
            self.assertEqual(handler.https_open(urllib.request.Request('https://productionresultssa16.blob.core.windows.net/')), 'constructed')

    def test_blob_compatibility_keeps_certificate_and_hostname_checks(self):
        context = context_for('productionresultssa16.blob.core.windows.net')
        self.assertEqual(context.minimum_version, ssl.TLSVersion.TLSv1_2)
        self.assertEqual(context.maximum_version, ssl.TLSVersion.TLSv1_2)
        self.assertEqual(context.verify_mode, ssl.CERT_REQUIRED)
        self.assertTrue(context.check_hostname)

    def test_other_endpoints_keep_modern_tls_negotiation(self):
        for host in ('api.github.com', 'blob.core.windows.net.evil.invalid', 'downloads.example.invalid'):
            context = context_for(host)
            self.assertNotEqual(context.maximum_version, ssl.TLSVersion.TLSv1_2)
            self.assertEqual(context.verify_mode, ssl.CERT_REQUIRED)
            self.assertTrue(context.check_hostname)
