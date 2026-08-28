import { describe, expect, test } from "bun:test";

describe("BucketMux AssemblyScript guest", () => {
  test("loads in Bun 1.4 and emits ABI v1 JSON", async () => {
    expect(Bun.version.startsWith("1.4.")).toBe(true);
    const bytes = await Bun.file(new URL("build/image-classifier.wasm", import.meta.url)).arrayBuffer();
    let memory: WebAssembly.Memory;
    let output = "";
    const imports = {
      env: {
        abort(): never {
          throw new Error("AssemblyScript guest aborted");
        },
        trace() {},
        seed() { return 0; }
      },
      wasi_snapshot_preview1: {
        fd_write(fd: number, iovs: number, count: number, written: number): number {
          if (fd !== 1) return 8;
          const view = new DataView(memory.buffer);
          let total = 0;
          for (let index = 0; index < count; index++) {
            const pointer = view.getUint32(iovs + index * 8, true);
            const length = view.getUint32(iovs + index * 8 + 4, true);
            output += new TextDecoder().decode(new Uint8Array(memory.buffer, pointer, length));
            total += length;
          }
          view.setUint32(written, total, true);
          return 0;
        }
      }
    };
    const instantiated = await WebAssembly.instantiate(bytes, imports);
    memory = instantiated.instance.exports.memory as WebAssembly.Memory;
    (instantiated.instance.exports._start as () => void)();
    const result = JSON.parse(output);
    expect(result.abi_version).toBe("bucketmux.wasm.v1");
    expect(result.tags["media-category"]).toBe("image");
    expect(result.operations[0].type).toBe("tags.patch");
  });
});
