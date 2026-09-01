package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"mbgw/internal/integration"
)

// api_channel_check.go — POST /api/vkm-channels/check-history: проверяет
// напрямую в САМОЙ ЭС, есть ли уже данные в указанных номерах каналов —
// защита от случайного назначения канала, принадлежащего СОВСЕМ ДРУГОЙ,
// посторонней точке ЭС (не одному из наших приборов). В отличие от
// проверки в handleVKMChannels (api_devices.go), которая знает только про
// каналы, уже настроенные у нас самих, — эта спрашивает саму ЭС и ловит
// вообще любой чужой канал, даже если мы о нём никогда раньше не слышали.
//
// Требует уже настроенное и рабочее подключение к БД ЭС (вкладка
// «Подключение к ЭС») — если оно не настроено, возвращает понятную
// причину пропуска, а не падает.
func (s *Server) handleCheckChannelHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "используйте POST")
		return
	}
	var body struct {
		ChannelIDs []int `json:"channel_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}

	conn, found, err := s.repo.GetESConnection(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось прочитать настройки подключения: "+err.Error())
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, map[string]any{
			"checked": false,
			"reason":  "подключение к БД ЭС ещё не настроено — проверка каналов на вкладке «Подключение к ЭС» пропущена",
		})
		return
	}

	writer, err := integration.OpenPointMainsWriter(integration.SQLServerConfig{
		Server: conn.SQLServer, Database: conn.SQLDatabase,
		User: conn.SQLUser, Password: conn.SQLPassword, Port: conn.SQLPort,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"checked": false, "reason": "не удалось подключиться к БД ЭС: " + err.Error()})
		return
	}
	defer writer.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	type occupiedChannel struct {
		ChannelID int    `json:"channel_id"`
		RowCount  int    `json:"row_count"`
		Oldest    string `json:"oldest"`
		Newest    string `json:"newest"`
	}
	var occupied []occupiedChannel
	for _, ch := range body.ChannelIDs {
		if ch == 0 {
			continue
		}
		rowCount, oldest, newest, has, err := writer.CheckPointHistory(ctx, ch)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"checked": false,
				"reason":  "ошибка проверки канала " + strconv.Itoa(ch) + ": " + err.Error(),
			})
			return
		}
		if has {
			occupied = append(occupied, occupiedChannel{
				ChannelID: ch, RowCount: rowCount,
				Oldest: oldest.Format("02.01.2006 15:04"),
				Newest: newest.Format("02.01.2006 15:04"),
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"checked": true, "occupied": occupied})
}
