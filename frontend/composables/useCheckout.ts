import type { components } from '~/types/legacy';

type OrderResponse = components["schemas"]["OrderResp"];
type PayResponse = components["schemas"]["PayOrderResp"];

export function useCheckout() {
  const processingId = ref<string | null>(null);

  async function checkout(type: "membership" | "credit_package", refId: string) {
    const config = useRuntimeConfig();
    processingId.value = refId;
    try {
      const order = await useApi()<OrderResponse>("/orders", {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: { type, refId }
      });
      if (!order.orderId) throw new Error("订单响应缺少 orderId");
      const channel = config.public.paymentChannel;
      const payment = await useApi()<PayResponse>(`/orders/${order.orderId}/pay`, {
        method: "POST",
        body: { channel }
      });
      if (channel === "dev") {
        await useApi()(`/orders/${order.orderId}/dev-complete`, { method: "POST" });
      } else {
        const redirectUrl = (payment.payParams as { redirectUrl?: string } | undefined)?.redirectUrl;
        if (redirectUrl && import.meta.client) window.location.assign(redirectUrl);
      }
      return order;
    } finally {
      processingId.value = null;
    }
  }

  return { processingId, checkout };
}
