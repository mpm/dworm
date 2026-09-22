"""Exercise the real installer against disposable new and legacy archives."""
import hashlib
import io
import os
from pathlib import Path
import platform
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class InstallerTest(unittest.TestCase):
    def test_archive_layouts(self):
        for legacy in (False, True):
            with self.subTest(legacy=legacy), tempfile.TemporaryDirectory() as tmp:
                base = Path(tmp)
                fixtures = base / "fixtures"
                fixtures.mkdir()
                osname = {"Linux": "linux", "Darwin": "darwin"}[platform.system()]
                arch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()]
                name = f"dworm_v0.6.0_{osname}_{arch}"
                archive = fixtures / (name + ".tar.gz")
                with tarfile.open(archive, "w:gz") as tar:
                    for binary in (["dworm", "dworm_endpoint"] if legacy else ["dworm"]):
                        data = b"#!/bin/sh\necho fixture\n"
                        info = tarfile.TarInfo(f"{name}/{binary}")
                        info.size = len(data)
                        info.mode = 0o755
                        tar.addfile(info, io.BytesIO(data))
                (fixtures / "checksums.txt").write_text(
                    f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}\n"
                )
                tools = base / "tools"
                tools.mkdir()
                curl = tools / "curl"
                curl.write_text('#!/bin/sh\ncp "$FIXTURES/${2##*/}" "$4"\n')
                curl.chmod(0o755)
                install = base / "install"
                install.mkdir()
                # New archives must leave any existing endpoint untouched.
                (install / "dworm_endpoint").write_text("old endpoint")
                env = dict(os.environ, PATH=f"{tools}:{os.environ['PATH']}",
                           FIXTURES=str(fixtures), DWORM_INSTALL_DIR=str(install))
                subprocess.run(["sh", str(ROOT / "install.sh"), "v0.6.0"], env=env, check=True,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                self.assertEqual(subprocess.check_output([str(install / "dworm")]), b"fixture\n")
                if legacy:
                    self.assertEqual(subprocess.check_output([str(install / "dworm_endpoint")]), b"fixture\n")
                else:
                    self.assertEqual((install / "dworm_endpoint").read_text(), "old endpoint")


if __name__ == "__main__":
    unittest.main()
