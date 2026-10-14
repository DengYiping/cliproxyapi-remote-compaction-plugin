"""Package only explicitly listed public build artifacts (never keys/configs)."""
import hashlib
import pathlib
import sys
import tarfile

root=pathlib.Path(__file__).resolve().parents[1]
version=sys.argv[1]
dist=root/'dist'
library=dist/f'remote-compaction-v{version}.dylib'
binary=dist/'remote-compaction'
archive=dist/f'remote-compaction-v{version}-darwin-arm64.tar.gz'
with tarfile.open(archive,'w:gz') as bundle:
    for path,name in [(library,f'darwin/arm64/{library.name}'),(binary,'bin/remote-compaction'),(root/'README.md','README.md')]:
        info=bundle.gettarinfo(str(path),arcname=name)
        info.uid=info.gid=0
        info.uname=info.gname=''
        with path.open('rb') as source:bundle.addfile(info,source)
checksum=dist/'SHA256SUMS'
checksum.write_text(''.join(hashlib.sha256(p.read_bytes()).hexdigest()+'  '+p.name+'\n' for p in [library,binary,archive]))
print('Packaged macOS arm64 library, key utility, and archive with SHA-256 checksums.')
