package update

// zstd extraction (PLAN-M2 3.7 step 4). The release artifact
// simplek8s.<ts>.<arch>.kernel.zst is a single zstd stream; it is
// decompressed to the stored kernel name (the .zst stripped).

import (
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

// extractZstd decompresses the zstd artifact at srcPath into dstPath
// (created/truncated, 0644).
func extractZstd(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open artifact: %w", err)
	}
	defer src.Close()

	dec, err := zstd.NewReader(src)
	if err != nil {
		return fmt.Errorf("zstd reader: %w", err)
	}
	defer dec.Close()

	out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("open target: %w", err)
	}
	if _, err := io.Copy(out, dec); err != nil {
		out.Close()
		return fmt.Errorf("zstd decompress: %w", err)
	}
	return out.Close()
}
