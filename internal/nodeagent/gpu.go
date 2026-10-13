package nodeagent

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

type GPUStats struct {
	TotalVRAMMB uint64
	FreeVRAMMB  uint64
	UtilPercent float64
	TempC       uint32
	ModelName   string
}

// ReadNvidiaSMI parses `nvidia-smi --query-gpu=memory.total,memory.free,utilization.gpu,temperature.gpu,name --format=csv,noheader,nounits`
func ReadNvidiaSMI(ctx context.Context) (*GPUStats, error) {
	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=memory.total,memory.free,utilization.gpu,temperature.gpu,name",
		"--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return nil, nil
	}

	// Aggregate across all detected GPUs
	var totalMB, freeMB uint64
	var maxUtil float64
	var maxTemp uint32
	var names []string

	for _, line := range lines {
		parts := strings.Split(line, ",")
		if len(parts) < 5 {
			continue
		}

		tot, _ := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 64)
		free, _ := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		util, _ := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
		temp, _ := strconv.ParseUint(strings.TrimSpace(parts[3]), 10, 32)
		name := strings.TrimSpace(parts[4])

		totalMB += tot
		freeMB += free
		if util > maxUtil {
			maxUtil = util
		}
		if uint32(temp) > maxTemp {
			maxTemp = uint32(temp)
		}
		names = append(names, name)
	}

	return &GPUStats{
		TotalVRAMMB: totalMB,
		FreeVRAMMB:  freeMB,
		UtilPercent: maxUtil,
		TempC:       maxTemp,
		ModelName:   strings.Join(names, ", "),
	}, nil
}
