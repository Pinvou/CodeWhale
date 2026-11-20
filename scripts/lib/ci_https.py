"""Storage-only TLS compatibility for CI's Python artifact clients."""
import http.client
import ssl
import urllib.parse
import urllib.request


def context_for(host):
    context = ssl.create_default_context()
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    if host.lower().endswith('.blob.core.windows.net'):
        context.maximum_version = ssl.TLSVersion.TLSv1_2
    return context


class StorageHTTPSHandler(urllib.request.HTTPSHandler):
    def https_open(self, request):
        host = urllib.parse.urlsplit(request.full_url).hostname or ''
        return self.do_open(http.client.HTTPSConnection, request,
                            context=context_for(host))


def storage_opener(*handlers):
    return urllib.request.build_opener(StorageHTTPSHandler(), *handlers)
