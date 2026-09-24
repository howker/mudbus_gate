package web

import (
	"context"
	"fmt"
	"strconv"

	sqliterepo "mbgw/internal/storage/sqlite"
)

func configuredVKMPipes(ctx context.Context, repo *sqliterepo.Repo, deviceID string) ([]int, error) {
	pipes, err := repo.GetVKMActivePipes(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	if len(pipes) == 0 {
		return []int{1}, nil
	}
	for _, pipe := range pipes {
		if pipe < 1 || pipe > 10 {
			return nil, fmt.Errorf("VKM pipe %d is outside 1..10", pipe)
		}
	}
	return pipes, nil
}

func vkmArchiveChannel(pipe int) string {
	if pipe <= 1 {
		return ""
	}
	return strconv.Itoa(pipe)
}

func containsPipe(pipes []int, wanted int) bool {
	for _, pipe := range pipes {
		if pipe == wanted {
			return true
		}
	}
	return false
}
