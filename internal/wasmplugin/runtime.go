package wasmplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/assemblyscript"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

const (
	wasmPageBytes      = int64(65536)
	maxResultJSONBytes = int64(1 << 20)
	maxStderrBytes     = int64(64 << 10)
	maxDerivedObjects  = 32
	maxEmbeddings      = 128
	maxEmbeddingDims   = 4096
	maximumMemoryPages = uint32(65536)
)

type Runtime struct {
	tempRoot string
}

// Executor is the narrow adapter seam used by the durable pipeline. A future
// out-of-process/GPU implementation can preserve job semantics without
// leaking its transport into object placement.
type Executor interface {
	Validate(context.Context, []byte, domain.WASMPlugin) error
	Execute(context.Context, []byte, domain.WASMPlugin, domain.WASMPluginInvocation, io.Reader) (*Execution, error)
}

type Execution struct {
	Result    domain.WASMPluginResult
	OutputDir string
	workDir   string
}

func NewRuntime(tempRoot string) *Runtime {
	return &Runtime{tempRoot: tempRoot}
}

var _ Executor = (*Runtime)(nil)

func (e *Execution) Close() error {
	if e == nil || e.workDir == "" {
		return nil
	}
	return os.RemoveAll(e.workDir)
}

func (e *Execution) OpenDerived(relativePath string) (*os.File, error) {
	path, err := safeOutputPath(e.OutputDir, relativePath)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}

func (r *Runtime) Validate(ctx context.Context, module []byte, plugin domain.WASMPlugin) error {
	if len(module) < 8 || !bytes.Equal(module[:4], []byte{'\x00', 'a', 's', 'm'}) {
		return errors.New("module is not a WebAssembly binary")
	}
	runtime, compiled, err := compile(ctx, module, plugin.MemoryLimitBytes)
	if err != nil {
		return err
	}
	defer func() {
		_ = compiled.Close(ctx)
		_ = runtime.Close(ctx)
	}()
	return validateCompiledModule(compiled)
}

func validateCompiledModule(compiled wazero.CompiledModule) error {
	if _, ok := compiled.ExportedFunctions()["_start"]; !ok {
		return errors.New("WASI command must export _start")
	}
	for _, function := range compiled.ImportedFunctions() {
		moduleName, name, imported := function.Import()
		if imported && moduleName != wasi_snapshot_preview1.ModuleName && (moduleName != "env" || !allowedAssemblyScriptImport(name)) {
			return fmt.Errorf("unsupported import %s.%s: only WASI Preview 1 is allowed", moduleName, name)
		}
	}
	if len(compiled.ImportedMemories()) != 0 {
		return errors.New("imported host memory is not allowed")
	}
	return nil
}

func (r *Runtime) Execute(ctx context.Context, module []byte, plugin domain.WASMPlugin, invocation domain.WASMPluginInvocation, input io.Reader) (*Execution, error) {
	timeout := time.Duration(plugin.TimeoutMillis) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	workDir, err := os.MkdirTemp(r.tempRoot, "bucketmux-wasm-")
	if err != nil {
		return nil, fmt.Errorf("create wasm workspace: %w", err)
	}
	execution := &Execution{workDir: workDir, OutputDir: filepath.Join(workDir, "output")}
	fail := func(err error) (*Execution, error) {
		_ = execution.Close()
		return nil, err
	}
	inputDir := filepath.Join(workDir, "input")
	if err := os.MkdirAll(inputDir, 0o750); err != nil {
		return fail(fmt.Errorf("create wasm input: %w", err))
	}
	if err := os.MkdirAll(execution.OutputDir, 0o750); err != nil {
		return fail(fmt.Errorf("create wasm output: %w", err))
	}
	inputFile, err := os.OpenFile(filepath.Join(inputDir, "object"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o400)
	if err != nil {
		return fail(fmt.Errorf("create wasm object input: %w", err))
	}
	written, copyErr := copyWithLimit(inputFile, input, plugin.MaxInputBytes)
	closeErr := inputFile.Close()
	if copyErr != nil {
		return fail(copyErr)
	}
	if closeErr != nil {
		return fail(closeErr)
	}
	if invocation.Object.Size >= 0 && written != invocation.Object.Size {
		return fail(fmt.Errorf("source size changed while preparing plugin input: read %d bytes, expected %d", written, invocation.Object.Size))
	}
	invocation.ABIVersion = domain.WASMPluginABIV1
	invocation.Object.InputPath = "/input/object"
	invocation.Workspace.OutputDir = "/output"
	invocation.Capabilities.Operations = plugin.OperationPolicy
	requestJSON, err := json.Marshal(invocation)
	if err != nil {
		return fail(fmt.Errorf("encode plugin invocation: %w", err))
	}

	runtime, compiled, err := compile(runCtx, module, plugin.MemoryLimitBytes)
	if err != nil {
		return fail(err)
	}
	defer func() {
		_ = compiled.Close(context.Background())
		_ = runtime.Close(context.Background())
	}()
	if err := validateCompiledModule(compiled); err != nil {
		return fail(fmt.Errorf("validate module: %w", err))
	}
	if _, err := wasi_snapshot_preview1.Instantiate(runCtx, runtime); err != nil {
		return fail(fmt.Errorf("instantiate WASI: %w", err))
	}
	if _, err := assemblyscript.Instantiate(runCtx, runtime); err != nil {
		return fail(fmt.Errorf("instantiate AssemblyScript imports: %w", err))
	}
	stdout := newLimitedBuffer(maxResultJSONBytes)
	stderr := newLimitedBuffer(maxStderrBytes)
	moduleConfig := wazero.NewModuleConfig().
		WithName("").
		WithArgs("bucketmux-plugin").
		WithStdin(bytes.NewReader(requestJSON)).
		WithStdout(stdout).
		WithStderr(stderr).
		WithFSConfig(wazero.NewFSConfig().
			WithReadOnlyDirMount(inputDir, "/input").
			WithDirMount(execution.OutputDir, "/output"))
	_, err = runtime.InstantiateModule(runCtx, compiled, moduleConfig)
	if err != nil {
		var exitErr *sys.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 0 {
			message := strings.TrimSpace(stderr.String())
			if message != "" {
				return fail(fmt.Errorf("execute wasm plugin: %w: %s", err, message))
			}
			return fail(fmt.Errorf("execute wasm plugin: %w", err))
		}
	}
	if stdout.overflowed {
		return fail(fmt.Errorf("plugin result exceeds %d bytes", maxResultJSONBytes))
	}
	if err := json.Unmarshal(stdout.Bytes(), &execution.Result); err != nil {
		return fail(fmt.Errorf("decode plugin result: %w", err))
	}
	if execution.Result.ABIVersion != domain.WASMPluginABIV1 {
		return fail(fmt.Errorf("plugin result abi_version %q does not match %q", execution.Result.ABIVersion, domain.WASMPluginABIV1))
	}
	if err := ValidateOperations(plugin, invocation.Object, execution.Result.Operations); err != nil {
		return fail(err)
	}
	if len(execution.Result.DerivedObjects) > maxDerivedObjects {
		return fail(fmt.Errorf("plugin declared %d derived objects; maximum is %d", len(execution.Result.DerivedObjects), maxDerivedObjects))
	}
	if err := validateEmbeddings(execution.Result.Embeddings); err != nil {
		return fail(err)
	}
	if err := validateOutputs(execution.OutputDir, execution.Result.DerivedObjects, plugin.MaxOutputBytes); err != nil {
		return fail(err)
	}
	return execution, nil
}

func validateEmbeddings(embeddings []domain.WASMPluginEmbedding) error {
	if len(embeddings) > maxEmbeddings {
		return fmt.Errorf("plugin declared %d embeddings; maximum is %d", len(embeddings), maxEmbeddings)
	}
	for i := range embeddings {
		embedding := &embeddings[i]
		embedding.Kind = strings.TrimSpace(embedding.Kind)
		embedding.Model = strings.TrimSpace(embedding.Model)
		embedding.ModelVersion = strings.TrimSpace(embedding.ModelVersion)
		embedding.Metric = strings.ToLower(strings.TrimSpace(embedding.Metric))
		if embedding.Kind == "" {
			embedding.Kind = "generic"
		}
		if embedding.Metric == "" {
			embedding.Metric = "cosine"
		}
		if embedding.Model == "" || embedding.ModelVersion == "" {
			return fmt.Errorf("embedding %d requires model and model_version", i)
		}
		if embedding.Metric != "cosine" && embedding.Metric != "dot" && embedding.Metric != "l2" {
			return fmt.Errorf("embedding %d uses unsupported metric %q", i, embedding.Metric)
		}
		if embedding.Dimensions == 0 {
			embedding.Dimensions = len(embedding.Values)
		}
		if embedding.Dimensions <= 0 || embedding.Dimensions > maxEmbeddingDims || embedding.Dimensions != len(embedding.Values) {
			return fmt.Errorf("embedding %d has dimensions %d and %d values; maximum dimensions is %d", i, embedding.Dimensions, len(embedding.Values), maxEmbeddingDims)
		}
		for j, value := range embedding.Values {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return fmt.Errorf("embedding %d value %d is not finite", i, j)
			}
		}
		if embedding.Metric == "cosine" {
			var norm float64
			for _, value := range embedding.Values {
				norm += float64(value) * float64(value)
			}
			if norm == 0 {
				return fmt.Errorf("embedding %d is a zero vector, which cosine search cannot index", i)
			}
		}
		if embedding.Metadata == nil {
			embedding.Metadata = map[string]string{}
		}
	}
	return nil
}

func compile(ctx context.Context, module []byte, memoryLimitBytes int64) (wazero.Runtime, wazero.CompiledModule, error) {
	pages := uint32((memoryLimitBytes + wasmPageBytes - 1) / wasmPageBytes)
	if pages == 0 {
		pages = 1024
	}
	if pages > maximumMemoryPages {
		pages = maximumMemoryPages
	}
	config := wazero.NewRuntimeConfig().
		WithMemoryLimitPages(pages).
		WithCloseOnContextDone(true).
		WithDebugInfoEnabled(false)
	runtime := wazero.NewRuntimeWithConfig(ctx, config)
	compiled, err := runtime.CompileModule(ctx, module)
	if err != nil {
		_ = runtime.Close(ctx)
		return nil, nil, fmt.Errorf("compile wasm module: %w", err)
	}
	return runtime, compiled, nil
}

func allowedAssemblyScriptImport(name string) bool {
	return name == "abort" || name == "trace" || name == "seed"
}

func copyWithLimit(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	if limit <= 0 {
		return 0, errors.New("plugin input limit must be positive")
	}
	written, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if err != nil {
		return written, fmt.Errorf("copy plugin input: %w", err)
	}
	if written > limit {
		return written, fmt.Errorf("plugin input exceeds %d bytes", limit)
	}
	return written, nil
}

func validateOutputs(outputDir string, declared []domain.WASMPluginDerivedObject, limit int64) error {
	if limit <= 0 {
		return errors.New("plugin output limit must be positive")
	}
	declaredPaths := make(map[string]bool, len(declared))
	for _, output := range declared {
		path, err := safeOutputPath(outputDir, output.Path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("stat derived object %q: %w", output.Path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("derived object %q is not a regular file", output.Path)
		}
		declaredPaths[path] = true
	}
	var total int64
	err := filepath.WalkDir(outputDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("plugin output contains a symbolic link: %s", path)
		}
		if !declaredPaths[path] {
			return fmt.Errorf("plugin wrote undeclared output %q", path)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if total > limit {
			return fmt.Errorf("plugin output exceeds %d bytes", limit)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("validate plugin output: %w", err)
	}
	return nil
}

func safeOutputPath(outputDir, relativePath string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(relativePath))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe derived object path %q", relativePath)
	}
	full := filepath.Join(outputDir, clean)
	relative, err := filepath.Rel(outputDir, full)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("derived object path escapes output directory: %q", relativePath)
	}
	return full, nil
}

type limitedBuffer struct {
	buffer     bytes.Buffer
	remaining  int64
	overflowed bool
}

func newLimitedBuffer(limit int64) *limitedBuffer {
	return &limitedBuffer{remaining: limit}
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if int64(len(data)) > b.remaining {
		allowed := int(b.remaining)
		if allowed > 0 {
			_, _ = b.buffer.Write(data[:allowed])
			b.remaining = 0
		}
		b.overflowed = true
		return len(data), nil
	}
	_, _ = b.buffer.Write(data)
	b.remaining -= int64(len(data))
	return len(data), nil
}

func (b *limitedBuffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *limitedBuffer) String() string { return b.buffer.String() }
