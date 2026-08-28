@external("wasi_snapshot_preview1", "fd_write")
declare function fdWrite(fd: i32, iovs: usize, iovsLength: i32, written: usize): i32;

function writeStdout(text: string): void {
  const encoded = String.UTF8.encode(text);
  const iovec = new Uint32Array(2);
  iovec[0] = <u32>changetype<usize>(encoded);
  iovec[1] = encoded.byteLength;
  const written = new Uint32Array(1);
  const errno = fdWrite(1, iovec.dataStart, 1, written.dataStart);
  if (errno != 0) unreachable();
}

export function _start(): void {
  writeStdout(
    '{"abi_version":"bucketmux.wasm.v1","metadata":{"classifier":"assemblyscript-bun-1.4-demo"},"tags":{"media-category":"image","safe-search":"not-evaluated"}}'
  );
}
