# Gemini liveness test

Throwaway PR to check whether Gemini Code Assist reviews again after quota reset + Standard subscription.

```go
func ratio(a, b int) int { return a + b } // BUG: adds instead of dividing; also no zero check
```
