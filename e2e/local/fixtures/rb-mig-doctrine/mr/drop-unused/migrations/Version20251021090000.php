<?php

declare(strict_types=1);

namespace DoctrineMigrations;

use Doctrine\DBAL\Schema\Schema;
use Doctrine\Migrations\AbstractMigration;

final class Version20251021090000 extends AbstractMigration
{
    public function getDescription(): string
    {
        return 'Drop unused orders.old_flag';
    }

    public function up(Schema $schema): void
    {
        $this->addSql('ALTER TABLE orders DROP COLUMN old_flag');
    }

    public function down(Schema $schema): void
    {
        $this->addSql('ALTER TABLE orders ADD old_flag BOOLEAN DEFAULT NULL');
    }
}
