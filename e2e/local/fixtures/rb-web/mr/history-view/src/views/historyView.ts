import { getOrderHistory } from "../api/history";

export async function showHistory(root: HTMLElement, id: number): Promise<void> {
  const h = await getOrderHistory(id);
  root.replaceChildren(
    ...h.history.map((status) => {
      const li = document.createElement("li");
      li.textContent = status;
      return li;
    }),
  );
}
