import { useCallback, useEffect, useState } from 'react';
import { loadBalancingApi, RoutingStrategy, RoutingStrategyResponse } from '@/services/api/loadBalancing';

export const STRATEGY_OPTIONS: { value: RoutingStrategy['strategy']; labelKey: string; descKey: string }[] = [
  { value: 'round-robin', labelKey: 'load_balancing.strategy_round_robin', descKey: 'load_balancing.strategy_round_robin_desc' },
  { value: 'fill-first', labelKey: 'load_balancing.strategy_fill_first', descKey: 'load_balancing.strategy_fill_first_desc' },
  { value: 'weighted-round-robin', labelKey: 'load_balancing.strategy_weighted', descKey: 'load_balancing.strategy_weighted_desc' },
  { value: 'latency-aware', labelKey: 'load_balancing.strategy_latency', descKey: 'load_balancing.strategy_latency_desc' },
  { value: 'adaptive', labelKey: 'load_balancing.strategy_adaptive', descKey: 'load_balancing.strategy_adaptive_desc' },
];

export function useRoutingStrategy() {
  const [strategy, setStrategy] = useState<RoutingStrategy['strategy']>('round-robin');
  const [availableStrategies, setAvailableStrategies] = useState<string[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const loadStrategy = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data: RoutingStrategyResponse = await loadBalancingApi.getRoutingStrategy();
      setStrategy(data.strategy);
      if (data.available_strategies) {
        setAvailableStrategies(data.available_strategies);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load strategy');
    } finally {
      setLoading(false);
    }
  }, []);

  const updateStrategy = useCallback(async (newStrategy: RoutingStrategy['strategy']) => {
    setLoading(true);
    setError(null);
    try {
      await loadBalancingApi.updateRoutingStrategy(newStrategy);
      setStrategy(newStrategy);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to update strategy');
      throw err;
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    loadStrategy();
  }, [loadStrategy]);

  return { strategy, availableStrategies, loading, error, updateStrategy, reload: loadStrategy };
}
