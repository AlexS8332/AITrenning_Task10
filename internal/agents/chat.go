package agents

import (
	"context"

	"github.com/AlexS8332/AITrenning_Task10/internal/agent"
)

// Chat — агент без инструментов: та же история диалога, ответы из памяти
// модели. Точка отсчёта: по нему видно, что даёт история сама по себе,
// а что — инструменты.
type Chat struct {
	deps Deps
}

func NewChat(deps Deps) agent.Agent {
	return &Chat{deps: deps}
}

func (c *Chat) Name() string { return "chat" }

const chatSystem = `Ты справочник по животным и ведёшь диалог с пользователем.

` + dialogRules + `

Рассказывая о животном, начни с общепринятого русского названия и латинского названия в скобках. Если запрос не про реальное животное или название тебе неизвестно, так и скажи одной фразой, не придумывай и не подменяй запрос похожим животным.`

func (c *Chat) Reply(ctx context.Context, in agent.Input, em agent.Emitter) (agent.Reply, error) {
	spec := agent.Spec{
		Name:     c.Name(),
		System:   chatSystem,
		MaxSteps: 1,
	}
	return run(ctx, c.deps, spec, in, em, "чат без инструментов")
}
