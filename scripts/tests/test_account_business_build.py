from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parents[2]


class AccountBusinessBuildTests(unittest.TestCase):
    def test_download_is_independent_of_service_and_revision(self):
        source = (ROOT / 'account-platform/Dockerfile').read_text()
        download = source.index('go mod download')
        for argument in ('ARG SERVICE', 'ARG VERSION=dev', 'ARG COMMIT=unknown'):
            self.assertGreater(source.index(argument), download)
        self.assertIn('ARG GO_BUILD_IMAGE=golang:1.26.5-alpine', source)
        self.assertIn('FROM ${GO_BUILD_IMAGE} AS build', source)

    def test_module_cache_is_shared_and_build_cache_is_not_packaged(self):
        source = (ROOT / 'account-platform/Dockerfile').read_text()
        module = '--mount=type=cache,id=pinvou-account-platform-gomod-go126,target=/go/pkg/mod,sharing=locked'
        self.assertEqual(source.count(module), 2)
        self.assertIn('--mount=type=cache,id=pinvou-account-platform-gobuild-go126,target=/root/.cache/go-build,sharing=locked', source)
        runtime = source.split('FROM scratch\n', 1)[1]
        self.assertNotIn('/go/pkg/mod', runtime)
        self.assertNotIn('/root/.cache', runtime)
        self.assertIn('ENV GOMEMLIMIT=1GiB GOMAXPROCS=2', source)
        self.assertNotIn('GOMEMLIMIT', runtime)
        self.assertIn('USER 65532:65532', runtime)
        self.assertIn('ENTRYPOINT ["/service"]', runtime)
        for path in ('service', 'healthcheck', 'token-policy-bootstrap'):
            self.assertIn('COPY --from=build /out/' + path + ' /' + path, runtime)
        self.assertIn('COPY --from=build /src/migrations /migrations', runtime)

    def test_exact_production_service_targets_exist(self):
        services = ('account-service', 'identity-service', 'billing-service', 'token-gateway', 'token-portal',
                    'token-credential-portal', 'token-command-executor', 'event-relay', 'migrate', 'event-resources',
                    'account-security', 'token-governance', 'token-release-prepare', 'hold-recovery', 'event-replay')
        self.assertEqual(len(set(services)), 15)
        for service in services:
            self.assertTrue((ROOT / 'account-platform/cmd' / service / 'main.go').is_file(), service)


if __name__ == '__main__':
    unittest.main()
