// Package agent adapts anything invokable on a Case: openai-compatible (one
// base_url-configurable adapter covering OpenAI, Together, Fireworks, Groq,
// vLLM, and Ollama), anthropic, http, and shell.
//
// Injection is a capability, not a requirement. Adapters advertise what they
// support through Capabilities, the engine degrades per-adapter rather than
// failing, and every valuation is labeled with the mode actually used —
// context_injection is an upper bound, knowledge_add is deployment-faithful.
package agent
