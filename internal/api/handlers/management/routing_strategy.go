package management

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Supported routing strategies
const (
	StrategyRoundRobin         = "round-robin"
	StrategyFillFirst          = "fill-first"
	StrategyWeightedRoundRobin = "weighted-round-robin"
	StrategyLatencyAware       = "latency-aware"
	StrategyAdaptive           = "adaptive"
)

var supportedStrategies = map[string]bool{
	StrategyRoundRobin:         true,
	StrategyFillFirst:          true,
	StrategyWeightedRoundRobin: true,
	StrategyLatencyAware:       true,
	StrategyAdaptive:           true,
}

// GetRoutingStrategy returns the current routing strategy.
// GET /v0/management/routing-strategy
func (h *Handler) GetRoutingStrategy(c *gin.Context) {
	fmt.Printf("DEBUG GetRoutingStrategy called!\n")
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config not available"})
		return
	}

	strategy := h.cfg.Routing.Strategy
	if strategy == "" {
		strategy = StrategyRoundRobin // default
	}

	c.JSON(http.StatusOK, gin.H{
		"strategy": strategy,
	})
}

// PutRoutingStrategyRequest represents the request body for updating routing strategy.
type PutRoutingStrategyRequest struct {
	Strategy string `json:"strategy"`
}

// PutRoutingStrategy updates the routing strategy.
// PUT /v0/management/routing-strategy
func (h *Handler) PutRoutingStrategy(c *gin.Context) {
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config not available"})
		return
	}

	var req PutRoutingStrategyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	strategy := req.Strategy
	if strategy == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "strategy is required"})
		return
	}

	if !supportedStrategies[strategy] {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":                "invalid strategy",
			"supported_strategies": []string{StrategyRoundRobin, StrategyFillFirst, StrategyWeightedRoundRobin, StrategyLatencyAware, StrategyAdaptive},
		})
		return
	}

	h.cfg.Routing.Strategy = strategy
	h.persist(c)
}
