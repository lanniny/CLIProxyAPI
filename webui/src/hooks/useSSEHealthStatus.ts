import { useEffect, useState, useCallback, useRef } from 'react';
import { useAuthStore } from '@/stores';
import { HealthStatus, CircuitBreakerStatus } from '@/services/api/loadBalancing';

const MAX_RECONNECT_DELAY = 30000; // 30 seconds
const INITIAL_RECONNECT_DELAY = 1000; // 1 second

export function useSSEHealthStatus() {
  const [healthStatus, setHealthStatus] = useState<HealthStatus[]>([]);
  const [circuitBreakers, setCircuitBreakers] = useState<CircuitBreakerStatus[]>([]);
  const [connected, setConnected] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const eventSourceRef = useRef<EventSource | null>(null);
  const reconnectAttemptRef = useRef(0);
  const reconnectTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const apiBase = useAuthStore((state) => state.apiBase);
  const managementKey = useAuthStore((state) => state.managementKey);

  const disconnect = useCallback(() => {
    if (reconnectTimeoutRef.current) {
      clearTimeout(reconnectTimeoutRef.current);
      reconnectTimeoutRef.current = null;
    }
    if (eventSourceRef.current) {
      eventSourceRef.current.close();
      eventSourceRef.current = null;
    }
    setConnected(false);
  }, []);

  const connect = useCallback(() => {
    if (!apiBase || !managementKey) {
      return;
    }

    // Clean up existing connection
    if (eventSourceRef.current) {
      eventSourceRef.current.close();
    }

    const normalizedBase = apiBase.replace(/\/?v0\/management\/?$/i, '');
    const url = `${normalizedBase}/v0/management/health-status/stream?key=${encodeURIComponent(managementKey)}`;

    const eventSource = new EventSource(url);
    eventSourceRef.current = eventSource;

    eventSource.onopen = () => {
      setConnected(true);
      setError(null);
      reconnectAttemptRef.current = 0; // Reset reconnect attempts on successful connection
    };

    // Listen for named events from backend
    eventSource.addEventListener('health-update', (event: MessageEvent) => {
      try {
        const data = JSON.parse(event.data);
        setHealthStatus(data);
      } catch (err) {
        console.error('Failed to parse health-update data:', err);
      }
    });

    eventSource.addEventListener('circuit-breaker-update', (event: MessageEvent) => {
      try {
        const data = JSON.parse(event.data);
        setCircuitBreakers(data);
      } catch (err) {
        console.error('Failed to parse circuit-breaker-update data:', err);
      }
    });

    // Fallback for generic message events (backward compatibility)
    eventSource.onmessage = (event) => {
      try {
        const data = JSON.parse(event.data);
        // Try to determine the type based on data structure
        if (Array.isArray(data) && data.length > 0) {
          if ('healthy' in data[0]) {
            setHealthStatus(data);
          } else if ('state' in data[0] && 'failure_count' in data[0]) {
            setCircuitBreakers(data);
          }
        }
      } catch (err) {
        console.error('Failed to parse SSE data:', err);
      }
    };

    eventSource.onerror = () => {
      setConnected(false);
      eventSource.close();

      // Implement exponential backoff for reconnection
      const delay = Math.min(
        INITIAL_RECONNECT_DELAY * Math.pow(2, reconnectAttemptRef.current),
        MAX_RECONNECT_DELAY
      );

      setError(`Connection lost. Reconnecting in ${Math.round(delay / 1000)}s...`);
      reconnectAttemptRef.current++;

      reconnectTimeoutRef.current = setTimeout(() => {
        connect();
      }, delay);
    };
  }, [apiBase, managementKey]);

  useEffect(() => {
    connect();

    return () => {
      disconnect();
    };
  }, [connect, disconnect]);

  // Manual reconnect function
  const reconnect = useCallback(() => {
    reconnectAttemptRef.current = 0;
    disconnect();
    connect();
  }, [connect, disconnect]);

  return {
    healthStatus,
    circuitBreakers,
    connected,
    error,
    reconnect,
    disconnect
  };
}
