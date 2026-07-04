package device

import (
    "context"
    "testing"

    "mbgw/internal/profile"
    "mbgw/internal/session"
    "mbgw/internal/storage/sqlite"
)

type mockClient struct{}

func (m *mockClient) ReadRaw(ctx context.Context, space string, addr int, dataType string) ([]byte, error) {
    // митируем ответ чистого PDU (123.4567).
    return []byte{0x42, 0xF6, 0xE9, 0xD5}, nil
}

func (m *mockClient) Transact(ctx context.Context, req []byte) ([]byte, error) {
    // Not exercised by TestDevicePoll (current-values path only); archive
    // polling tests will need a more capable fake once wired into Device.
    return nil, nil
}

func TestDevicePoll(t *testing.T) {
    p := &profile.Profile{
        Codec: profile.Codec{WordOrder32: "0123"},
        Points: []profile.Point{
            {Name: "Тестовый расход", Type: "float", Unit: "кг/с"},
        },
    }
    cli := &mockClient{}
    sess := &session.NoopSession{}
    repo, _ := sqlite.New("test_worker.json")
    dev := New("vkm_test", p, cli, sess, repo)
    dev.poll(context.Background())
}