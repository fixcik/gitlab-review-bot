import type { User } from "../api/client";

// Renders a user card; the email is shown as a mailto link.
export function renderUserCard(user: User): HTMLElement {
  const card = document.createElement("article");
  card.className = "user-card";

  const name = document.createElement("h3");
  name.textContent = user.name || user.email;
  card.appendChild(name);

  const mail = document.createElement("a");
  mail.href = `mailto:${user.email}`;
  mail.textContent = user.email.toLowerCase();
  card.appendChild(mail);

  return card;
}
