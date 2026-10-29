# frozen_string_literal: true

# Bootstrap of the local GitLab for reviewbot e2e checks. Run by up.sh:
#
#   docker compose exec -T -e RB_MODE=tokens gitlab \
#     gitlab-rails runner /opt/reviewbot-local/bootstrap.rb
#
# Idempotent: users and groups are created only when missing, memberships are
# (re)applied. With RB_MODE=tokens it also (re)creates the personal access
# tokens named "reviewbot-local" (old ones with that name are revoked) and
# resets the root password.
#
# Secrets are written to STDOUT only, as lines "RBOUT KEY=value", which up.sh
# captures into e2e/local/.env.local without echoing them. Progress and errors
# go to STDERR and never contain secrets.

require 'securerandom'

MODE = ENV.fetch('RB_MODE', 'ensure')
TOKEN_NAME = 'reviewbot-local'
TOKEN_DAYS = Integer(ENV.fetch('RB_TOKEN_DAYS', '90'))
GROUP = 'reviewbot-sandbox'
GROUP_EXT = 'reviewbot-sandbox-ext'

# username => [display name, access level in GROUP]
USERS = {
  'reviewbot' => ['Review Bot', Gitlab::Access::REPORTER],
  'rb-dev' => ['RB Developer', Gitlab::Access::DEVELOPER],
  'rb-guest' => ['RB Guest', Gitlab::Access::GUEST]
}.freeze

def log(msg)
  warn "bootstrap: #{msg}"
end

def out(key, value)
  $stdout.puts "RBOUT #{key}=#{value}"
end

# Passwords GitLab accepts: long, mixed, no username or common words.
def random_password
  "#{SecureRandom.alphanumeric(20)}-#{SecureRandom.alphanumeric(8)}!7a"
end

# The default organization (GitLab 17+ requires one for users, groups and,
# in newer versions, tokens). nil on versions without organizations.
def organization_id
  return @organization_id if defined?(@organization_id)

  @organization_id =
    begin
      if defined?(Organizations::Organization) && Organizations::Organization.respond_to?(:default_organization)
        Organizations::Organization.default_organization&.id
      end
    rescue StandardError => e
      log "no default organization (#{e.class})"
      nil
    end
end

def service_payload(result, key)
  return result if result.is_a?(ActiveRecord::Base)
  return result.payload[key] if result.respond_to?(:payload)
  return result[key] if result.respond_to?(:[])

  nil
end

def ensure_user(root, username, name)
  user = User.find_by_username(username)
  if user
    log "user #{username} exists"
    return user
  end

  password = random_password
  params = {
    username: username, name: name, email: "#{username}@example.com",
    password: password, password_confirmation: password,
    skip_confirmation: true
  }
  params[:organization_id] = organization_id if organization_id
  result = Users::CreateService.new(root, params).execute
  user = service_payload(result, :user)
  unless user&.persisted?
    msg = result.respond_to?(:message) ? result.message : user&.errors&.full_messages&.join(', ')
    raise "cannot create user #{username}: #{msg}"
  end
  user.update_column(:password_expires_at, nil) if user.respond_to?(:password_expires_at)
  log "user #{username} created"
  user
end

def ensure_group(root, path)
  group = Group.find_by_full_path(path)
  if group
    log "group #{path} exists"
    return group
  end

  params = { name: path, path: path, visibility_level: Gitlab::VisibilityLevel::PRIVATE }
  params[:organization_id] = organization_id if organization_id
  result = Groups::CreateService.new(root, params).execute
  group = service_payload(result, :group) || Group.find_by_full_path(path)
  raise "cannot create group #{path}: #{group&.errors&.full_messages&.join(', ')}" unless group&.persisted?

  log "group #{path} created"
  group
end

def ensure_member(group, user, level)
  member = group.members.find_by(user_id: user.id)
  if member && member.access_level == level
    return
  end

  group.add_member(user, level)
  log "#{user.username}: access level #{level} in #{group.full_path}"
end

# Revokes earlier tokens of this stand and creates a new api token.
def create_token(user, scopes)
  user.personal_access_tokens.where(name: TOKEN_NAME, revoked: false).find_each do |t|
    t.revoke!
  end
  params = { name: TOKEN_NAME, scopes: scopes, expires_at: TOKEN_DAYS.days.from_now.to_date }

  if defined?(PersonalAccessTokens::CreateService)
    begin
      # Keyword names differ between versions (organization_id appeared in 17.x).
      names = PersonalAccessTokens::CreateService.instance_method(:initialize).parameters.map(&:last)
      kw = { current_user: user, target_user: user, params: params }
      kw[:organization_id] = organization_id if names.include?(:organization_id)
      result = PersonalAccessTokens::CreateService.new(**kw).execute
      token = service_payload(result, :personal_access_token)
      return token.token if result.respond_to?(:success?) && result.success? && token&.token

      log "PersonalAccessTokens::CreateService for #{user.username}: #{result.respond_to?(:message) ? result.message : 'failed'}; falling back"
    rescue StandardError => e
      log "PersonalAccessTokens::CreateService for #{user.username}: #{e.class}; falling back"
    end
  end

  attrs = params.dup
  attrs[:organization_id] = organization_id if organization_id && PersonalAccessToken.column_names.include?('organization_id')
  token = user.personal_access_tokens.create!(attrs)
  token.token
end

root = User.find_by_username('root') || User.admins.first
raise 'no admin user' unless root

log "GitLab #{Gitlab::VERSION}, mode #{MODE}"

users = USERS.to_h { |username, (name, _)| [username, ensure_user(root, username, name)] }

sandbox = ensure_group(root, GROUP)
ensure_group(root, GROUP_EXT)
USERS.each { |username, (_, level)| ensure_member(sandbox, users[username], level) }
# The bot gets Reporter on reviewbot-sandbox-ext/ext-client only (seed.sh,
# project level); no group membership in reviewbot-sandbox-ext.

if MODE == 'tokens'
  password = random_password
  root.password = password
  root.password_confirmation = password
  root.password_automatically_set = false if root.respond_to?(:password_automatically_set=)
  root.save!
  root.update_column(:password_expires_at, nil) if root.respond_to?(:password_expires_at)
  out 'GITLAB_ROOT_PASSWORD', password

  out 'GITLAB_SERVICE_TOKEN', create_token(root, %w[api])
  out 'GITLAB_TOKEN', create_token(users['reviewbot'], %w[api])
  out 'RB_DEV_TOKEN', create_token(users['rb-dev'], %w[api])
  out 'RB_GUEST_TOKEN', create_token(users['rb-guest'], %w[api])
  log 'tokens created'
end

out 'GITLAB_VERSION_RUNNING', Gitlab::VERSION
log 'done'
